// model_catalog_single.go re-discovers ONE account's model catalog and makes the
// host re-register that credential, from the panel's per-account 刷新 button.
//
// Why this exists (2026-10-22): the host only calls model.for_auth when a
// credential is (re-)registered. An account that stays unused for a long time --
// typically one parked at zero credits -- therefore keeps whatever catalog it
// registered with, even after upstream starts advertising new models for it.
// Observed live: a workbuddy global-realm account had no bare
// "deepseek-v4.1-flash" in its registered catalog while every other account did,
// so a staged rule naming it in stage 1 silently skipped it and traffic fell
// through to stage 2 forever. Disabling and re-enabling the account fixed it
// instantly, which proved the catalog was stale rather than upstream omitting the
// model. qoder has the same exposure: discovery is cached per account and the
// host only re-reads it on registration.
//
// The panel's 刷新 button used to refresh credits only, so the one button an
// operator reaches for could not fix that state. It now refreshes both.
//
// Two steps are required and neither alone is enough:
//  1. callModelsAPI re-pulls this account's upstream catalog into the plugin's
//     per-account cache (what the panel renders, and what model.for_auth serves).
//  2. Saving the auth file back (same storage, same metadata) makes the host
//     re-run registration for that credential, which is the only moment it calls
//     model.for_auth again and so rebuilds the registry the router reads.
//
// Step 2 is a same-content write: it bumps mtime, which is what the host's
// watcher keys on. It deliberately does NOT toggle disabled, so the account never
// leaves the routing pool and there is no availability gap.
package main

import (
	"encoding/json"
	"strings"
)

// callModelsAPIFn is the discovery entry point, injectable so tests can drive
// the upstream-failure branch without a live gateway.
var callModelsAPIFn = callModelsAPI

// refreshAccountModelCatalog re-discovers one account's catalog and asks the host
// to re-register it. Best-effort by design: the caller is a UI refresh, and a
// failed upstream pull must not turn a working 刷新 button into an error.
//
// It returns a short human-readable status for the panel:
//
//	"refreshed"     catalog re-pulled and re-registration triggered
//	"catalog-only"  catalog re-pulled, but the host re-register step failed
//	"not-ready"     upstream pull failed and no earlier catalog is usable
func refreshAccountModelCatalog(authIndex string) string {
	sa, phys, err := panelHostAuthBundle(authIndex)
	if err != nil || sa == nil {
		return "not-ready"
	}
	dyn, err := callModelsAPIFn(sa)
	if err != nil || len(dyn) == 0 {
		// Nothing usable came back. Do NOT re-register: publishing an empty
		// catalog would drop the account from routing, which is worse than
		// leaving the last good registration alone.
		return "not-ready"
	}
	// Persist the freshly discovered catalog next to the in-memory cache, so a
	// restart does not immediately fall back to the previous generation.
	//
	// callModelsAPI only returns model IDs: it does not parse multipliers,
	// context tiers or thinking_config. Passing nil here used to overwrite the
	// stored region_facts with nothing, so every 刷新 click silently erased the
	// account's capability data. Inherit the existing facts instead and let the
	// next full discovery replace them.
	factors, facts := existingModelCatalogFacts(sa)
	persistModelCatalogFor(sa, dyn, factors, facts)
	status := "refreshed"
	if err := triggerHostAuthReregister(phys); err != nil {
		status = "catalog-only"
	}
	return status
}

// triggerHostAuthReregister makes the host re-run registration for one credential
// by saving its auth file back unchanged.
//
// Every top-level key not owned by this function is inherited from the file on
// disk (buildAuthFileJSONFrom), so disabled_reason / note / label survive the
// round-trip.
func triggerHostAuthReregister(phys *hostAuthPhysical) error {
	if phys == nil {
		return errPhysicalAuthMissing{}
	}
	sa, err := parseStored(phys.JSON)
	if err != nil || sa == nil {
		return err
	}
	name := strings.TrimSpace(phys.Name)
	if name == "" {
		name = authFileNameFor(sa)
	}
	// Inherit the current document: only the storage blob is rewritten.
	var base map[string]any
	if len(phys.JSON) > 0 {
		_ = json.Unmarshal(phys.JSON, &base)
	}
	disabled := false
	if raw, ok := base["disabled"].(bool); ok {
		disabled = raw
	}
	note, _ := base["note"].(string)
	raw, err := buildAuthFileJSONFrom(sa, disabled, note, nil, phys.JSON)
	if err != nil {
		return err
	}
	return hostAuthSaveJSONFn(name, raw)
}

// errPhysicalAuthMissing reports a credential the host knows but cannot hand back
// as a file (runtime-only entries). Re-registration is impossible for those.
type errPhysicalAuthMissing struct{}

func (errPhysicalAuthMissing) Error() string { return "auth record has no physical file" }
