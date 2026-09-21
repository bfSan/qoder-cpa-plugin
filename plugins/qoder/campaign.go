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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type campaignStatusResponse struct {
	ShowCampaign bool       `json:"showCampaign"`
	Claimable    bool       `json:"claimable"`
	Campaigns    []campaign `json:"campaigns"`
}

type campaign struct {
	CampaignID  string       `json:"campaignId"`
	CampaignKey string       `json:"campaignKey"`
	ActionType  string       `json:"actionType"`
	StartAt     int64        `json:"startAt"`
	EndAt       int64        `json:"endAt"`
	ClaimStatus string       `json:"claimStatus"`
	Benefit     *campaignBen `json:"benefit,omitempty"`
}

type campaignBen struct {
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

func fetchCampaignStatus(sa *storedAuth) (*campaignStatusResponse, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/sash/api/v1/me/campaigns?forceRefresh=true", nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("campaigns http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var out campaignStatusResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("campaigns parse: %w", err)
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

func claimedCampaign(status *campaignStatusResponse) *campaign {
	if status == nil {
		return nil
	}
	for i := range status.Campaigns {
		c := &status.Campaigns[i]
		if strings.EqualFold(c.ActionType, "CLAIM_BENEFIT") && strings.EqualFold(c.ClaimStatus, "CLAIMED") {
			return c
		}
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
	}
	return sum
}

func campaignCredit(c *campaign) int64 {
	if c == nil || c.Benefit == nil || !strings.EqualFold(c.Benefit.Kind, "CREDITS") {
		return 0
	}
	return c.Benefit.Amount
}

// performCampaignCheckin claims one Intl campaign and normalizes the response
// to the same shape as the CN daily-check-in claim.
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
	req, err := http.NewRequest(
		http.MethodPost,
		upstreamBaseFor(sa)+"/sash/api/v1/me/campaigns/"+c.CampaignID+"/claim",
		strings.NewReader("{}"),
	)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	// The activity page accepts either a bare payload or a {data:{...}} envelope.
	body := m
	if data, ok := m["data"].(map[string]any); ok {
		body = data
	}
	if statusValue, _ := body["status"].(string); !strings.EqualFold(statusValue, "CLAIMED") {
		return map[string]any{"success": false, "upstream": m}, nil
	}
	if replayed, _ := body["replayed"].(bool); replayed {
		return map[string]any{
			"success":       false,
			"result":        "ALREADY_CLAIMED",
			"rewardCredits": float64(campaignCredit(c)),
		}, nil
	}
	return map[string]any{
		"success":        true,
		"result":         "CLAIMED",
		"rewardCredits":  float64(campaignCredit(c)),
		"campaign_id":    c.CampaignID,
		"campaign_key":   c.CampaignKey,
		"campaign_title": c.CampaignKey,
	}, nil
}
