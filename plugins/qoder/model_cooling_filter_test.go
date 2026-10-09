package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The panel has always shown per-(account, model) throttling, but model.for_auth
// ignored it, so CPA kept treating a throttled pair as healthy. With
// routing.session-affinity enabled that meant a session stayed pinned to the
// throttled pair for the whole TTL instead of failing over. These tests pin the
// filter that closes that gap, and the reason it matters is that CPA registers
// exactly this response against the auth and then skips accounts whose
// registration lacks the requested model.

// servingModelIDsFor runs model.for_auth for one auth and returns the model IDs
// CPA would register.
func servingModelIDsFor(t *testing.T, authID string, models []string) []string {
	t.Helper()
	sa := testStoredAuthCNAccount()
	sa.Account.UID = authID
	infos := make([]pluginapi.ModelInfo, 0, len(models))
	for _, id := range models {
		infos = append(infos, pluginapi.ModelInfo{ID: id, Name: id, OwnedBy: providerName})
	}
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{
		accountCacheKey(sa): infos,
	})
	defer restore()

	storage, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelForAuth(mustJSONBytes(t, pluginapi.AuthModelRequest{
		AuthID:      authID,
		StorageJSON: storage,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("for_auth envelope = %#v", env)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hasModelID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestQoderForAuthWithholdsOnlyTheCoolingModel(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-cooling-auth"
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	ids := servingModelIDsFor(t, authID, []string{"alpha", "beta", "gamma"})
	if hasModelID(ids, "beta") {
		t.Fatalf("cooling model still served to CPA: %v", ids)
	}
	// Only the failing pair is withheld. cooldown.go documents why: cooling the
	// whole credential turns one degraded model into an auth-wide outage, which
	// is worst when a single Qoder auth is configured.
	if !hasModelID(ids, "alpha") || !hasModelID(ids, "gamma") {
		t.Fatalf("healthy models were dropped along with the cooling one: %v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("served %v, want alpha and gamma only", ids)
	}
}

func TestQoderForAuthIgnoresAnotherAccountsCooldown(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-mine"
	markModelCooldown("qoder-other", "beta", cooldownReasonRateLimit)
	markModelCooldown("qoder-other", "alpha", cooldownReasonRateLimit)

	// Another account cooling these models says nothing about this one, and
	// applying it here would hide working models.
	ids := servingModelIDsFor(t, authID, []string{"alpha", "beta"})
	if len(ids) != 2 {
		t.Fatalf("another account's cooldown leaked into this auth: %v", ids)
	}
}

func TestQoderForAuthRestoresModelAfterCooldownExpires(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-expiry"
	base := time.Now()
	cooldownNowFn = func() time.Time { return base }
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	if ids := servingModelIDsFor(t, authID, []string{"alpha", "beta"}); hasModelID(ids, "beta") {
		t.Fatalf("cooling model served before expiry: %v", ids)
	}
	// A transient throttle must not permanently shrink the catalog.
	cooldownNowFn = func() time.Time { return base.Add(modelCooldownRateLimit + time.Minute) }
	if ids := servingModelIDsFor(t, authID, []string{"alpha", "beta"}); !hasModelID(ids, "beta") {
		t.Fatalf("model did not return after the cooldown expired: %v", ids)
	}
}

// Cooldowns are keyed by the routing model while this response carries catalog
// IDs, and CPA rewrites aliases in between. A cooldown recorded against the
// upstream name must still hide the catalog entry.
func TestQoderForAuthMatchesCoolingRecordedAgainstAlias(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-alias"
	markModelCooldown(authID, "upstream-model", cooldownReasonRateLimit)

	sa := testStoredAuthCNAccount()
	sa.Account.UID = authID
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{
		accountCacheKey(sa): {
			{ID: "alias-model", Name: "Alias", OwnedBy: providerName},
			{ID: "other-model", Name: "Other", OwnedBy: providerName},
		},
	})
	defer restore()
	storage, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelForAuth(mustJSONBytes(t, pluginapi.AuthModelRequest{
		AuthID:      authID,
		StorageJSON: storage,
		Attributes: map[string]string{
			"model_alias": `[{"alias":"alias-model","name":"upstream-model"}]`,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		ids = append(ids, m.ID)
	}
	if hasModelID(ids, "alias-model") {
		t.Fatalf("aliased cooling model was still served: %v", ids)
	}
	if !hasModelID(ids, "other-model") {
		t.Fatalf("unrelated model was dropped: %v", ids)
	}
}

// A cooldown matching nothing must not thin the catalog: a missed filter keeps
// today's behaviour, a false positive makes a working model vanish.
func TestQoderForAuthKeepsEveryModelWhenNothingMatches(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-nomatch"
	markModelCooldown(authID, "some-other-model", cooldownReasonRateLimit)

	ids := servingModelIDsFor(t, authID, []string{"alpha", "beta"})
	if len(ids) != 2 {
		t.Fatalf("unrelated cooldown removed models: %v", ids)
	}
}

// Without an auth ID there is nothing to scope the cooldown to, and matching
// against every account could hide a healthy model.
func TestQoderForAuthWithoutAuthIDKeepsCatalog(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("some-auth", "alpha", cooldownReasonRateLimit)
	models := []pluginapi.ModelInfo{{ID: "alpha"}, {ID: "beta"}}
	if got := filterCoolingModels(pluginapi.AuthModelRequest{}, models); len(got) != 2 {
		t.Fatalf("unscoped filter removed models: %#v", got)
	}
}

// Regression guard for the reason this filter exists: the panel's admin catalog
// and region view read the dynamic cache directly, and an operator has to keep
// seeing a cooling model in order to clear it.
func TestQoderCoolingDoesNotEmptyDynamicCache(t *testing.T) {
	resetCooldowns(t)
	const authID = "qoder-cache"
	sa := testStoredAuthCNAccount()
	sa.Account.UID = authID
	restore := setDynamicModelsCacheForTest(map[string][]pluginapi.ModelInfo{
		accountCacheKey(sa): {
			{ID: "alpha", Name: "Alpha"},
			{ID: "beta", Name: "Beta"},
		},
	})
	defer restore()
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	if ids := servingModelIDsFor(t, authID, []string{"alpha", "beta"}); hasModelID(ids, "beta") {
		t.Fatalf("precondition failed, beta should be withheld from CPA: %v", ids)
	}
	// The cache itself must be untouched, so adminModelCatalog / registry
	// statuses still describe beta.
	cached, ok := cachedDynamicModelsFor(accountCacheKey(sa))
	if !ok || len(cached) != 2 {
		t.Fatalf("cooldown filter mutated the dynamic cache: %#v", cached)
	}
}

func TestQoderCoolingModelSetForScopesAndSweeps(t *testing.T) {
	resetCooldowns(t)
	base := time.Now()
	cooldownNowFn = func() time.Time { return base }
	markModelCooldown("auth-a", "alpha", cooldownReasonRateLimit)
	markModelCooldown("auth-a", "beta", cooldownReasonRateLimit)
	markModelCooldown("auth-b", "gamma", cooldownReasonRateLimit)

	set := coolingModelSetFor("auth-a")
	if len(set) != 2 {
		t.Fatalf("auth-a set = %#v, want alpha and beta", set)
	}
	if _, ok := set["gamma"]; ok {
		t.Fatalf("auth-b leaked into auth-a's set: %#v", set)
	}
	// Expired entries must be swept, not reported.
	cooldownNowFn = func() time.Time { return base.Add(modelCooldownRateLimit + time.Minute) }
	if got := coolingModelSetFor("auth-a"); len(got) != 0 {
		t.Fatalf("expired entries still reported: %#v", got)
	}
}

// CPA decides the scope and duration of the cooldown it records from the status
// on the error, so the type has to expose one.
func TestQoderStatusErrorExposesStatusCode(t *testing.T) {
	err := &statusError{status: http.StatusTooManyRequests, err: errors.New("upstream 429")}
	var sc interface{ StatusCode() int }
	if !errors.As(err, &sc) {
		t.Fatal("statusError does not expose StatusCode")
	}
	if got := sc.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want 429", got)
	}
}
