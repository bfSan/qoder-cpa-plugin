// campaign.go implements the daily-benefit contract shared by CN and Intl.
//
//	GET  /sash/api/v1/me/campaigns
//	POST /sash/api/v1/me/campaigns/{campaignId}/claim
//
// The desktop client polls the same status endpoint and opens the activity
// page, which performs the claim through these two calls. CN moved from its
// legacy /me/daily-check-in pair to this contract on 2026-09-21.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type campaignStatusResponse struct {
	ShowCampaign bool       `json:"showCampaign"`
	Claimable    bool       `json:"claimable"`
	Campaigns    []campaign `json:"campaigns"`
}

type campaign struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	ClaimStatus string `json:"claimStatus"`
	// ClaimedAt is only present on /claim responses; the /me/campaigns list
	// omits it (verified live 2026-10-22). Kept here so a listing that starts
	// returning it needs no further plumbing.
	ClaimedAt string       `json:"claimedAt,omitempty"`
	Benefit   *campaignBen `json:"benefit,omitempty"`
}

type campaignBen struct {
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

func fetchCampaignStatus(sa *storedAuth) (*campaignStatusResponse, error) {
	resp, err := checkinHTTPRequest(sa, http.MethodGet, "/sash/api/v1/me/campaigns?forceRefresh=true")
	if err != nil {
		return nil, err
	}
	if err := checkinHTTPError(resp.StatusCode, false); err != nil {
		return nil, err
	}
	var out campaignStatusResponse
	if json.Unmarshal(resp.Body, &out) != nil {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	if out.Campaigns == nil {
		// Explicit false flags can describe an inactive response without a list.
		// Unlike {} or null this cannot authorize a POST or prove a claim.
		var flags struct {
			Show      *bool `json:"showCampaign"`
			Claimable *bool `json:"claimable"`
		}
		if json.Unmarshal(resp.Body, &flags) != nil || flags.Show == nil || flags.Claimable == nil || *flags.Show || *flags.Claimable {
			return nil, &checkinRequestError{Kind: "protocol"}
		}
		out.Campaigns = []campaign{}
	}
	now := time.Now().Unix()
	for _, c := range out.Campaigns {
		if !strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") || c.StartAt > now || c.EndAt > 0 && c.EndAt < now {
			continue
		}
		if c.CampaignID == "" || !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") && !strings.EqualFold(c.ClaimStatus, "CLAIMED") {
			return nil, &checkinRequestError{Kind: "protocol"}
		}
	}
	return &out, nil
}

// claimableCampaign returns the first CLAIM_BENEFIT campaign that is currently
// claimable and still inside its activity window.
func claimableCampaign(status *campaignStatusResponse) *campaign {
	if status == nil {
		return nil
	}
	now := time.Now().Unix()
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		if !strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") {
			continue
		}
		if !strings.EqualFold(c.ClaimStatus, "CLAIMABLE") {
			continue
		}
		if c.StartAt > 0 && now < c.StartAt {
			continue
		}
		if c.EndAt > 0 && now > c.EndAt {
			continue
		}
		return c
	}
	return nil
}

// claimedCampaign returns the first CLAIM_BENEFIT campaign that has already
// been claimed AND is still inside its own activity window.
//
// The window filter is essential, not cosmetic: the upstream /me/campaigns
// list keeps historical CLAIM_BENEFIT entries around after they expire, so an
// unfiltered scan matches yesterday's (or last week's) campaign forever. That
// made campaignCheckinSummary report TodayCheckedIn=true permanently, which in
// turn short-circuited checkinOneAccount into the "今日已签到" branch before it
// ever issued a POST /claim — the panel's manual check-in silently did nothing
// while the daily grant was in fact never claimed (2026-10-22 field report).
//
// This mirrors claimableCampaign's own StartAt/EndAt handling so the two
// halves of the decision agree on what "today" means.
func claimedCampaign(status *campaignStatusResponse) *campaign {
	if status == nil {
		return nil
	}
	now := time.Now().Unix()
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		if !strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") {
			continue
		}
		if !strings.EqualFold(c.ClaimStatus, "CLAIMED") {
			continue
		}
		if c.StartAt > 0 && now < c.StartAt {
			continue
		}
		if c.EndAt > 0 && now > c.EndAt {
			continue
		}
		return c
	}
	return nil
}

func fetchCampaignCheckinSummary(sa *storedAuth) (*checkinSummary, error) {
	status, err := fetchCampaignStatus(sa)
	if err != nil {
		return nil, err
	}
	return campaignCheckinSummary(status), nil
}

func campaignCheckinSummary(status *campaignStatusResponse) *checkinSummary {
	sum := &checkinSummary{
		ActivityName: "权益活动",
	}
	if status == nil {
		return sum
	}
	if c := claimableCampaign(status); c != nil {
		sum.Active = true
		sum.DailyCredit = campaignCredit(c)
		return sum
	}
	if c := claimedCampaign(status); c != nil {
		sum.Active = true
		sum.TodayCheckedIn = true
		sum.DailyCredit = campaignCredit(c)
		sum.TodayCredit = campaignCredit(c)
		sum.ClaimedAt = c.ClaimedAt
	}
	return sum
}

func campaignCredit(c *campaign) int64 {
	if c == nil || c.Benefit == nil || !strings.EqualFold(c.Benefit.Kind, "CREDITS") {
		return 0
	}
	return c.Benefit.Amount
}

// performCampaignCheckin preserves the Qoder contract: a structured CLAIMED
// receipt is authoritative even if /campaigns subsequently removes the entry.
func performCampaignCheckin(sa *storedAuth) (map[string]any, error) {
	status, err := fetchCampaignStatus(sa)
	if err != nil {
		return nil, err
	}
	c := claimableCampaign(status)
	if c == nil {
		if claimedCampaign(status) != nil {
			return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
		}
		return map[string]any{"success": false, "message": "当前没有可领取的活动"}, nil
	}
	return performCampaignClaim(sa, c)
}

func performCampaignClaim(sa *storedAuth, c *campaign) (map[string]any, error) {
	if c == nil || c.CampaignID == "" {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	resp, err := checkinHTTPRequest(sa, http.MethodPost, "/sash/api/v1/me/campaigns/"+url.PathEscape(c.CampaignID)+"/claim")
	if err != nil {
		return nil, err
	}
	var m map[string]any
	parseErr := json.Unmarshal(resp.Body, &m)
	if resp.StatusCode == http.StatusConflict && parseErr == nil && m["result"] == "ALREADY_CLAIMED" {
		return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
	}
	// Only a success HTTP response can carry an authoritative CLAIMED receipt.
	if err := checkinHTTPError(resp.StatusCode, true); err != nil {
		return nil, err
	}
	if parseErr != nil || m == nil {
		return nil, &checkinRequestError{Kind: "protocol", Uncertain: true, Retryable: true}
	}
	body := m
	if data, ok := m["data"].(map[string]any); ok {
		body = data
	}
	if err := checkinBodyRejection(body); err != nil {
		return nil, err
	}
	status, _ := body["status"].(string)
	if !strings.EqualFold(status, "CLAIMED") {
		if body["success"] == false {
			return map[string]any{"success": false}, &checkinRequestError{Kind: "business"}
		}
		return nil, &checkinRequestError{Kind: "protocol", Uncertain: true, Retryable: true}
	}
	out := map[string]any{"success": true, "result": "CLAIMED", "rewardCredits": float64(campaignCredit(c)), "campaign_id": c.CampaignID, "campaign_key": c.CampaignKey, "campaign_title": c.CampaignKey}
	if replayed, _ := body["replayed"].(bool); replayed {
		out["success"], out["result"], out["replayed"] = false, "ALREADY_CLAIMED", true
	}
	for from, to := range map[string]string{"claimedAt": "claimed_at", "grantId": "grant_id"} {
		if v, ok := body[from].(string); ok && v != "" {
			out[to] = v
		}
	}
	// An internal marker, not inferred from arbitrary success booleans/text.
	out["confirmed_receipt"] = true
	return out, nil
}

// The shared hardened route never replays failed bridge POSTs directly. This
// helper adds check-in-specific typed errors without bypassing routing policy.
// No credentials or request-scoped callback are stored in scheduled work.
// The direct route honors the context; Host RPC deadline/redirect policy is
// controlled by the Host because its HTTP wire has no per-request controls.
func checkinHTTPRequest(sa *storedAuth, method, path string) (*hostHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var body []byte
	if method == http.MethodPost {
		body = []byte("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, upstreamBaseFor(sa)+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, &checkinRequestError{Kind: "transport", Retryable: checkinTransportRetryable(err), Uncertain: method == http.MethodPost}
	}
	if resp == nil || resp.StatusCode == 0 {
		return nil, &checkinRequestError{Kind: "protocol", Retryable: method == http.MethodPost, Uncertain: method == http.MethodPost}
	}
	return resp, nil
}

type checkinRequestError struct {
	Kind      string
	Status    int
	Retryable bool
	Uncertain bool
}

func (e *checkinRequestError) Error() string {
	switch e.Kind {
	case "auth":
		return fmt.Sprintf("签到登录失效或无权限 (http %d)，不会自动重试", e.Status)
	case "transport":
		return "签到连接失败或超时，未确认成功"
	case "server":
		return "签到服务暂不可用，未确认成功"
	case "rate_limit":
		return "签到接口限流，未确认成功"
	case "not_found":
		return "签到接口不存在"
	case "protocol":
		return "签到响应状态未知，未确认成功"
	default:
		return "上游拒绝签到，请检查活动资格；不会自动重试"
	}
}

func checkinHTTPError(status int, post bool) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return &checkinRequestError{Kind: "auth", Status: status}
	case status == 404 || status == 405:
		return &checkinRequestError{Kind: "not_found", Status: status}
	case status == 429:
		return &checkinRequestError{Kind: "rate_limit", Status: status, Retryable: true}
	case status == http.StatusRequestTimeout:
		return &checkinRequestError{Kind: "transport", Status: status, Retryable: true, Uncertain: post}
	case post && status >= 300 && status < 400:
		// A redirect may follow a completed mutation. Do not replay the POST.
		return &checkinRequestError{Kind: "protocol", Status: status, Retryable: true, Uncertain: true}
	case status >= 500:
		return &checkinRequestError{Kind: "server", Status: status, Retryable: true, Uncertain: post}
	default:
		return &checkinRequestError{Kind: "business", Status: status}
	}
}

func checkinLegacyFallback(err error) bool {
	var ce *checkinRequestError
	return errors.As(err, &ce) && (ce.Status == 404 || ce.Status == 405) && !ce.Uncertain
}

func checkinTransportRetryable(err error) bool {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) || ue.Err == nil {
			break
		}
		err = ue.Err
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, text := range []string{"x509:", "certificate", "unsupported protocol", "invalid url", "invalid port", "no such host"} {
		if strings.Contains(low, text) {
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) {
		return true
	}
	for _, text := range []string{"timeout", "timed out", "deadline exceeded", "connection reset", "connection refused", "unexpected eof", "network is unreachable", "broken pipe"} {
		if strings.Contains(low, text) {
			return true
		}
	}
	return false
}

// Recognize only structured permanent rejection codes, never words in a
// message. This prevents a 200 auth/business response from consuming retries.
func checkinBodyRejection(body map[string]any) error {
	for _, key := range []string{"code", "errorCode", "status"} {
		value, _ := body[key].(string)
		switch strings.ToUpper(value) {
		case "UNAUTHORIZED", "UNAUTHENTICATED":
			return &checkinRequestError{Kind: "auth", Status: 401}
		case "FORBIDDEN":
			return &checkinRequestError{Kind: "auth", Status: 403}
		case "REJECTED", "FAILED", "NOT_ELIGIBLE", "EXPIRED":
			return &checkinRequestError{Kind: "business"}
		}
	}
	return nil
}
