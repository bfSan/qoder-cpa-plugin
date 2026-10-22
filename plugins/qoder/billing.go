// billing.go owns the upstream billing API surface: check-in status, user
// resource (credits / packages), payment type, and the perform-* call wrappers
// for daily check-in. Includes the shared JSON helpers used to tolerate the
// upstream's loosely-typed response shapes.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// billingClientType is the desktop client's Cosy-ClientType. Qoder's billing
// surface gates the campaign response on this header: the same credential
// that returns showCampaign:false to a bare request returns the live daily
// "100 Credits" campaign once Cosy-ClientType is present. Verified live
// 2026-09-21 against openapi.qoder.com.cn and openapi.qoder.sh.
const (
	billingClientType = "10"
	billingClientVer  = "0.3.4"
)

func billingHeaders(req *http.Request, sa *storedAuth) {
	// QoderWork billing endpoints authenticate with the active token as a
	// plain Bearer — jobToken (jt-) or device token (dt-), both accepted
	// upstream (verified live 2026-07-27). No COSY signing (KNOWLEDGE §2).
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Qoder")
	req.Header.Set("Cosy-ClientType", billingClientType)
	req.Header.Set("Cosy-Version", billingClientVer)
	// 2026-09-21: the billing surface additionally requires the web session
	// cookies (acw_tc / qoder_csrf_token) and the mirrored CSRF header. Without
	// them every /sash and /api/v2 call answers 401 "missing cookie header".
	applyBillingSessionHeaders(req, sa)
}

// checkinStatusResponse mirrors GET /sash/api/v1/me/daily-check-in/status
// (plain JSON, no envelope).
type checkinStatusResponse struct {
	Status             string `json:"status"` // CLAIMABLE | CLAIMED
	RewardCredits      int64  `json:"rewardCredits"`
	NextClaimAt        int64  `json:"nextClaimAt"` // epoch seconds from upstream
	CurrentStreakDays  int64  `json:"currentStreakDays"`
	TotalClaimDays     int64  `json:"totalClaimDays"`
	TotalRewardCredits int64  `json:"totalRewardCredits"`
	LastClaimedAt      int64  `json:"lastClaimedAt"`   // epoch seconds from upstream
	RewardExpiresAt    int64  `json:"rewardExpiresAt"` // epoch seconds from upstream
}

// checkinObservation binds a claim to the exact status/contract just read.
// An unknown status must never trigger a speculative POST or contract switch.
type checkinObservation struct {
	summary  *checkinSummary
	campaign *campaign
	legacy   bool
}

func fetchCheckinObservation(sa *storedAuth) (*checkinObservation, error) {
	if capabilitiesForRegion(authRegion(sa)).Contract == checkinContractCampaign {
		status, err := fetchCampaignStatus(sa)
		if err == nil {
			return &checkinObservation{summary: campaignCheckinSummary(status), campaign: claimableCampaign(status)}, nil
		}
		if authRegion(sa) != regionCN || !checkinLegacyFallback(err) {
			return nil, err
		}
	}
	ci, err := fetchDailyCheckinStatus(sa)
	if err != nil {
		return nil, err
	}
	return &checkinObservation{summary: ci, legacy: true}, nil
}

func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	observation, err := fetchCheckinObservation(sa)
	if err != nil {
		return nil, err
	}
	return observation.summary, nil
}

// fetchDailyCheckinStatus is the legacy CN daily-check-in contract, kept as a
// fallback after CN moved to /me/campaigns (2026-09-21).
func fetchDailyCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	resp, err := checkinHTTPRequest(sa, http.MethodGet, "/sash/api/v1/me/daily-check-in/status")
	if err != nil {
		return nil, err
	}
	if err := checkinHTTPError(resp.StatusCode, false); err != nil {
		return nil, err
	}
	var q checkinStatusResponse
	if json.Unmarshal(resp.Body, &q) != nil || (q.Status != "CLAIMABLE" && q.Status != "CLAIMED") {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	today := time.Now().Format("2006-01-02")
	lastClaimed := ""
	if q.LastClaimedAt > 0 {
		lastClaimed = time.Unix(q.LastClaimedAt, 0).Format("2006-01-02")
	}
	sum := &checkinSummary{
		Active:          q.Status == "CLAIMABLE" || q.Status == "CLAIMED",
		TodayCheckedIn:  q.Status == "CLAIMED" && lastClaimed == today,
		StreakDays:      q.CurrentStreakDays,
		DailyCredit:     q.RewardCredits,
		TodayCredit:     0,
		TotalCredits:    q.TotalRewardCredits,
		WeekCheckinDays: q.TotalClaimDays,
		ActivityName:    "每日签到",
	}
	if sum.TodayCheckedIn {
		sum.TodayCredit = q.RewardCredits
	}
	if q.Status == "CLAIMED" && !sum.TodayCheckedIn {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	return sum, nil
}

// quotaUsageResponse mirrors GET /api/v2/quota/usage response (plain JSON,
// no envelope). Both userQuota (base credits) and addOnQuota (one-time pro
// upgrade + checkin packs) are summed for the panel.
type quotaUsageResponse struct {
	UserID               string  `json:"userId"`
	UserType             string  `json:"userType"`
	UsageType            string  `json:"usageType"`
	TotalUsagePercentage float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded      bool    `json:"isQuotaExceeded"`
	ExpiresAt            int64   `json:"expiresAt"` // ms epoch
	UpgradeURL           string  `json:"upgradeUrl"`
	UserQuota            struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
		Unit      string  `json:"unit"`
	} `json:"userQuota"`
	// AddOnQuota is a pointer because the gateway omits it entirely (null) for an
	// account that has no add-on pack at all. Treating null as a zero-valued pack
	// makes "this account was never given a pack" indistinguishable from "the pack
	// is used up", and the two need different wording in the panel.
	AddOnQuota *struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
	} `json:"addOnQuota"`
}

// fetchUserResource queries QoderWork's quota endpoint and aggregates base +
// add-on credits into the panel's creditsSummary shape.
//
// fetchUserResourceFn is the indirection used by the management handlers so
// tests can exercise the credits path without network access.
var fetchUserResourceFn = fetchUserResource

func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/api/v2/quota/usage", nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("quota/usage http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var q quotaUsageResponse
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		return nil, fmt.Errorf("quota/usage parse: %w", err)
	}
	addOnRemain := int64(0)
	addOnUsed := int64(0)
	addOnTotal := int64(0)
	packCount := 1
	packages := []packageSummary{
		{Name: "基础额度", Remain: int64(q.UserQuota.Remaining), Used: int64(q.UserQuota.Used), Size: int64(q.UserQuota.Total)},
	}
	if q.AddOnQuota != nil {
		addOnRemain = int64(q.AddOnQuota.Remaining)
		addOnUsed = int64(q.AddOnQuota.Used)
		addOnTotal = int64(q.AddOnQuota.Total)
		packCount = 2
		packages = append(packages, packageSummary{
			Name: "赠送/签到额度", Remain: addOnRemain, Used: addOnUsed, Size: addOnTotal,
		})
	}
	sum := &creditsSummary{
		TotalRemain: int64(q.UserQuota.Remaining) + addOnRemain,
		TotalUsed:   int64(q.UserQuota.Used) + addOnUsed,
		TotalSize:   int64(q.UserQuota.Total) + addOnTotal,
		PackCount:   packCount,
		Packages:    packages,
		// Carry the gateway's own verdict so the panel can say "no quota" instead
		// of inferring exhaustion from a zero balance.
		QuotaExceeded: q.IsQuotaExceeded,
		NoAddOnPack:   q.AddOnQuota == nil,
	}
	return sum, nil
}

// planResponse mirrors GET /api/v2/user/plan (plain JSON, no envelope).
type planResponse struct {
	UserType       string          `json:"user_type"`
	PlanTierName   string          `json:"plan_tier_name"`
	IsPersonal     bool            `json:"is_personal_version"`
	IsPaid         bool            `json:"is_paid_plan"`
	IsHighestTier  bool            `json:"is_highest_tier"`
	FeatureAllowed map[string]bool `json:"feature_allowed"`
	StartDate      int64           `json:"start_date"` // ms epoch
	EndDate        int64           `json:"end_date"`   // ms epoch
}

// fetchPaymentTypeFn is the injectable indirection for plan lookups.
var fetchPaymentTypeFn = fetchPaymentType

func fetchPaymentType(sa *storedAuth) string {
	req, err := http.NewRequest(http.MethodGet, upstreamBaseFor(sa)+"/api/v2/user/plan", nil)
	if err != nil {
		return ""
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil || resp.StatusCode >= 400 {
		return ""
	}
	var p planResponse
	if err := json.Unmarshal(resp.Body, &p); err != nil {
		return ""
	}
	// Prefer plan_tier_name (e.g. "Pro Trial") over the raw user_type string.
	if p.PlanTierName != "" {
		return p.PlanTierName
	}
	return p.UserType
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	observation, err := fetchCheckinObservation(sa)
	if err != nil {
		return nil, err
	}
	if observation.summary.TodayCheckedIn {
		return map[string]any{"success": false, "result": "ALREADY_CLAIMED"}, nil
	}
	if !observation.summary.Active {
		return map[string]any{"success": false}, nil
	}
	if observation.legacy {
		return performDailyCheckinCall(sa)
	}
	return performCampaignClaim(sa, observation.campaign)
}

// The legacy acknowledgement still requires a status verification. Missing
// success, nil error, free text or a duplicate marker are not confirmation.
func performDailyCheckinCall(sa *storedAuth) (map[string]any, error) {
	resp, err := checkinHTTPRequest(sa, http.MethodPost, "/sash/api/v1/me/daily-check-in/claim")
	if err != nil {
		return nil, err
	}
	var m map[string]any
	parseErr := json.Unmarshal(resp.Body, &m)
	if resp.StatusCode == http.StatusConflict && parseErr == nil && m["result"] == "ALREADY_CLAIMED" {
		return m, nil
	}
	if err := checkinHTTPError(resp.StatusCode, true); err != nil {
		return nil, err
	}
	if parseErr != nil || m == nil {
		return nil, &checkinRequestError{Kind: "protocol", Retryable: true, Uncertain: true}
	}
	if err := checkinBodyRejection(m); err != nil {
		return nil, err
	}
	if _, ok := m["success"].(bool); !ok && m["result"] != "ALREADY_CLAIMED" {
		return nil, &checkinRequestError{Kind: "protocol", Retryable: true, Uncertain: true}
	}
	return m, nil
}

// isCreditsExhausted is the shared "耗尽" definition for the panel.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// An account the gateway reports as having no quota at all — no add-on pack,
	// nothing ever used and no capacity — has not run out of anything. Every
	// balance is zero because it was never given a pack, so treating this as
	// exhaustion parks a valid credential, and the operator cannot tell that
	// apart from a genuinely spent account.
	if cr.NoAddOnPack && cr.TotalUsed == 0 && cr.TotalSize == 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0 or size>0). pure zero with no real total = no data.
	return cr.TotalUsed > 0 || cr.TotalSize > 0
}
