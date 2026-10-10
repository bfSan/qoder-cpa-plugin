package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游对"从未下发额度包"的账号回 addOnQuota:null 且所有余额为 0。
// 这种账号什么都没用过，把它当成"耗尽"会把一个完全可用的凭证停用，
// 而操作者从面板上分不出它和"真的用完了"的区别。

func quotaBody(t *testing.T, addOn any, exceeded bool) []byte {
	t.Helper()
	body := map[string]any{
		"userId":               "u1",
		"userType":             "personal_standard",
		"totalUsagePercentage": 0,
		"isQuotaExceeded":      exceeded,
		"userQuota": map[string]any{
			"total": 0, "used": 0, "remaining": 0, "unit": "credits",
		},
	}
	if addOn != nil {
		body["addOnQuota"] = addOn
	} else {
		body["addOnQuota"] = nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fetchWithQuotaBody(t *testing.T, raw []byte) *creditsSummary {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	sa := testStoredAuthCNAccount()
	fillFixtureToken(sa)
	restore := setUpstreamBaseForTest(authRegion(sa), srv.URL)
	t.Cleanup(restore)
	cr, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	return cr
}

// addOnQuota 缺席时标出 NoAddOnPack，且只报一个额度包（不是两个假包）。
func TestQuotaWithoutAddOnPackIsFlagged(t *testing.T) {
	cr := fetchWithQuotaBody(t, quotaBody(t, nil, true))
	if !cr.NoAddOnPack {
		t.Fatal("a null addOnQuota must set NoAddOnPack")
	}
	if cr.PackCount != 1 {
		t.Fatalf("PackCount = %d, want 1 (only the base quota exists)", cr.PackCount)
	}
	if len(cr.Packages) != 1 {
		t.Fatalf("len(Packages) = %d, want 1", len(cr.Packages))
	}
	if !cr.QuotaExceeded {
		t.Fatal("isQuotaExceeded must be carried through")
	}
}

// 有额度包时行为不变：两个包、NoAddOnPack 为假。
func TestQuotaWithAddOnPackUnchanged(t *testing.T) {
	addOn := map[string]any{"total": 2000, "used": 573, "remaining": 1427}
	cr := fetchWithQuotaBody(t, quotaBody(t, addOn, false))
	if cr.NoAddOnPack {
		t.Fatal("a present addOnQuota must not set NoAddOnPack")
	}
	if cr.PackCount != 2 || len(cr.Packages) != 2 {
		t.Fatalf("PackCount=%d len(Packages)=%d, want 2/2", cr.PackCount, len(cr.Packages))
	}
	if cr.TotalRemain != 1427 {
		t.Fatalf("TotalRemain = %d, want 1427", cr.TotalRemain)
	}
}

// 无额度包 + 毫无用量 = 不是耗尽。这正是 Intl 账号被误停用的那条路径。
func TestNoQuotaPackIsNotExhaustion(t *testing.T) {
	cr := fetchWithQuotaBody(t, quotaBody(t, nil, true))
	if isCreditsExhausted(cr) {
		t.Fatal("an account with no quota pack must not read as exhausted")
	}
	if lifecycleActionFor(regionIntl, cr) != lifecycleNone {
		t.Fatal("lifecycle must not disable an account that has no quota pack")
	}
}

// 真的把钱花光（有已用量）仍然要算耗尽，否则停用逻辑就失效了。
func TestGenuineExhaustionStillDetected(t *testing.T) {
	addOn := map[string]any{"total": 2000, "used": 2000, "remaining": 0}
	cr := fetchWithQuotaBody(t, quotaBody(t, addOn, true))
	if !isCreditsExhausted(cr) {
		t.Fatal("a spent pack must still read as exhausted")
	}
	if lifecycleActionFor(regionCN, cr) != lifecycleDisable {
		t.Fatal("a spent account must still be parked")
	}
}

// 没有额度包时，note 不能写"耗尽 · 余0 已用0"——那读起来像"全花完了"。
func TestNoteDoesNotClaimExhaustionWithoutPack(t *testing.T) {
	cr := fetchWithQuotaBody(t, quotaBody(t, nil, true))
	sa := testStoredAuthCNAccount()
	note := displayNoteWithPrev(sa, cr, false, "")
	if strings.Contains(note, "耗尽") {
		t.Fatalf("note claims exhaustion with nothing ever used: %q", note)
	}
	if !strings.Contains(note, "无可用额度包") {
		t.Fatalf("note should say no pack was issued: %q", note)
	}
}
