package main

import "testing"

func TestRegionCapabilitiesMatchObservedUpstream(t *testing.T) {
	cn := capabilitiesForRegion(regionCN)
	if !cn.Checkin || !cn.ProUpgrade {
		t.Fatalf("CN capabilities = %#v, want check-in and pro upgrade", cn)
	}
	// CN moved to the campaign contract on 2026-09-21: the legacy
	// /me/daily-check-in endpoint answers DISABLED for a live account while
	// /me/campaigns reports the active "每天领 100 Credits" activity.
	if cn.Contract != checkinContractCampaign {
		t.Fatalf("CN contract = %v, want campaign", cn.Contract)
	}
	intl := capabilitiesForRegion(regionIntl)
	if !intl.Checkin {
		t.Fatalf("Intl capabilities = %#v, want check-in support (campaign live since 2026-10-21)", intl)
	}
	if intl.Contract != checkinContractCampaign {
		t.Fatalf("Intl contract = %v, want campaign", intl.Contract)
	}
	if intl.ProUpgrade {
		t.Fatalf("Intl capabilities = %#v, want pro upgrade unsupported", intl)
	}
	if !intl.Quota || !intl.Plan || !intl.Refresh {
		t.Fatalf("Intl capabilities = %#v, want quota/plan/refresh", intl)
	}
}

// supportsCheckin is the single gate used by runAutoCheckin, checkinOneAccount
// and the panel's detail cache. An Intl credential must pass it, otherwise the
// 10:00/21:00 timer silently skips the account and its daily campaign is never
// claimed.
func TestIntlCredentialSupportsCheckin(t *testing.T) {
	intl := &storedAuth{Auth: storedTokens{Region: regionIntl, Domain: domainIntl, AccessToken: "dt-test"}}
	if !supportsCheckin(intl) {
		t.Fatal("Intl account must support check-in")
	}
	// Legacy Intl credential without auth.region, identified by domain alone.
	legacy := &storedAuth{Auth: storedTokens{Domain: domainIntl, AccessToken: "dt-test"}}
	if !supportsCheckin(legacy) {
		t.Fatal("domain-only Intl credential must support check-in")
	}
	if authRegion(legacy) != regionIntl {
		t.Fatalf("authRegion(legacy) = %q, want intl", authRegion(legacy))
	}
}

func TestIntlCheckinStatusErrorIsRealError(t *testing.T) {
	err := classifyCheckinStatusError(regionIntl, 404, `{"errorCode":"NotFound"}`)
	if err == nil || isUnsupportedCheckinError(err) {
		t.Fatalf("error = %v, want real status error", err)
	}
}

func TestCNCheckinStatusErrorIsNotMaskedAsUnsupported(t *testing.T) {
	err := classifyCheckinStatusError(regionCN, 500, "boom")
	if isUnsupportedCheckinError(err) {
		t.Fatalf("CN 500 must remain a real error: %v", err)
	}
}
