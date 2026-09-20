package main

import "testing"

func TestRegionCapabilitiesMatchObservedUpstream(t *testing.T) {
	cn := capabilitiesForRegion(regionCN)
	if !cn.Checkin || !cn.ProUpgrade {
		t.Fatalf("CN capabilities = %#v, want check-in and pro upgrade", cn)
	}
	intl := capabilitiesForRegion(regionIntl)
	if !intl.Checkin {
		t.Fatalf("Intl capabilities = %#v, want campaign check-in support", intl)
	}
	if intl.ProUpgrade {
		t.Fatalf("Intl capabilities = %#v, want pro upgrade unsupported", intl)
	}
	if !intl.Quota || !intl.Plan || !intl.Refresh {
		t.Fatalf("Intl capabilities = %#v, want quota/plan/refresh", intl)
	}
}

func TestIntlCheckinStatusErrorIsRealError(t *testing.T) {
	err := classifyCheckinStatusError(regionIntl, 404, `{"errorCode":"NotFound"}`)
	if err == nil || isUnsupportedCheckinError(err) {
		t.Fatalf("error = %v, want real Intl check-in error", err)
	}
}

func TestCNCheckinStatusErrorIsNotMaskedAsUnsupported(t *testing.T) {
	err := classifyCheckinStatusError(regionCN, 500, "boom")
	if isUnsupportedCheckinError(err) {
		t.Fatalf("CN 500 must remain a real error: %v", err)
	}
}
