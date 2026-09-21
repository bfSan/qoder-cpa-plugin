// region_capability.go records which upstream billing contracts exist per
// region. CN and Intl share the quota/plan/user endpoints and the device-token
// family. Both regions deliver the daily benefit through the campaign
// contract:
//
//   - /sash/api/v1/me/campaigns
//   - POST /sash/api/v1/me/campaigns/{campaignId}/claim
//
// CN used to expose a dedicated /sash/api/v1/me/daily-check-in/{status,claim}
// pair. As of 2026-09-21 the CN account answers that endpoint with
// `{"campaignKey":"cn_daily_check_in_legacy","status":"DISABLED"}`, while the
// same account reports the live "每天领 100 Credits" activity through
// /me/campaigns. The desktop client already polls campaigns for CN, so the
// legacy contract is kept only as a fallback for older credentials.
//
// Intl has no Pro-upgrade contract. That absence is a capability fact, not a
// transient failure: the plugin must skip it instead of retrying a 404.
package main

import (
	"errors"
	"fmt"
)

type checkinContract int

const (
	checkinContractDaily    checkinContract = iota // legacy CN daily check-in
	checkinContractCampaign                        // campaign claim (CN + Intl)
)

type regionCapabilities struct {
	Checkin    bool
	ProUpgrade bool
	Quota      bool
	Plan       bool
	Refresh    bool
	Contract   checkinContract
}

func capabilitiesForRegion(region string) regionCapabilities {
	switch normalizeRegion(region) {
	case regionIntl:
		return regionCapabilities{
			Checkin:    true,
			ProUpgrade: false,
			Quota:      true,
			Plan:       true,
			Refresh:    true,
			Contract:   checkinContractCampaign,
		}
	default:
		return regionCapabilities{
			Checkin:    true,
			ProUpgrade: true,
			Quota:      true,
			Plan:       true,
			Refresh:    true,
			Contract:   checkinContractCampaign,
		}
	}
}

func supportsCheckin(sa *storedAuth) bool {
	return capabilitiesForRegion(authRegion(sa)).Checkin
}

func supportsProUpgrade(sa *storedAuth) bool {
	return capabilitiesForRegion(authRegion(sa)).ProUpgrade
}

type unsupportedCheckinError struct {
	Region string
	Status int
}

func (e *unsupportedCheckinError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("check-in is not supported in region %s (http %d)", e.Region, e.Status)
	}
	return "check-in is not supported in region " + e.Region
}

func isUnsupportedCheckinError(err error) bool {
	var target *unsupportedCheckinError
	return errors.As(err, &target)
}

func classifyCheckinStatusError(region string, status int, body string) error {
	if status >= 400 {
		return fmt.Errorf("checkin status http %d body=%s", status, truncateRedacted(body, 200))
	}
	return nil
}
