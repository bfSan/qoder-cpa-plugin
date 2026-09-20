// region_capability.go records which upstream billing contracts exist per
// region. CN and Intl share the quota/plan/user endpoints and the device-token
// family. Both regions offer a check-in, but through different contracts:
//
//   - CN: /sash/api/v1/me/daily-check-in/{status,claim}
//   - Intl: /sash/api/v1/me/campaigns + /{campaignId}/claim
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
	checkinContractDaily    checkinContract = iota // CN daily check-in
	checkinContractCampaign                        // Intl campaign claim
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
			Contract:   checkinContractDaily,
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
