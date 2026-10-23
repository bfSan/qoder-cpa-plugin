package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// redirectUserConfigDir pins os.UserConfigDir to a temp dir on both POSIX
// (XDG_CONFIG_HOME / HOME) and Windows (APPDATA) so defaultQoderModelStore
// never touches the real CLIProxyAPI config directory during tests.
func redirectUserConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
}

func catalogFixture(region, uid string, fetched time.Time) modelCatalogCacheV1 {
	factor := 1.5
	ctxTokens := int64(200000)
	return modelCatalogCacheV1{
		SchemaVersion:  modelCatalogSchemaVersion,
		IdentitySHA256: modelCatalogIdentitySHA256(region, uid),
		Region:         region,
		UID:            uid,
		FetchedAt:      fetched.UTC(),
		Models: []pluginapi.ModelInfo{
			{ID: "gm51model", Name: "GM51", OwnedBy: region},
			{ID: "qwen3.8-flash", Name: "Qwen Flash"},
		},
		Factors: map[string]float64{"gm51model": 1.5, "qwen3.8-flash": 0},
		RegionFacts: map[string]modelRegionFacts{
			"gm51model": {
				PriceFactor:   &factor,
				ContextLength: &ctxTokens,
				IsFree:        false,
				ContextTiers:  []contextTier{{Label: "200K", Tokens: 200000, IsDefault: true}},
			},
			"qwen3.8-flash": {IsFree: true},
		},
	}
}

func TestModelCatalogSaveLoadRoundTrip(t *testing.T) {
	store := &qoderModelStore{root: filepath.Join(t.TempDir(), "model-catalog")}
	want := catalogFixture(regionCN, "uid-1", time.Now().Add(-time.Hour))
	if err := store.saveModelCatalog(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := filepath.Join(store.root, want.IdentitySHA256+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no POSIX permission bits: os.Chmod can only toggle the
	// read-only attribute, so a 0o600 write reads back as 0o666. Assert the
	// mode where the operating system can actually represent it.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file mode = %o, want 600", perm)
		}
	}
	list, err := store.loadModelCatalogAll()
	if err != nil || len(list) != 1 {
		t.Fatalf("loadAll = %d entries, err %v", len(list), err)
	}
	got := list[0]
	if got.Region != want.Region || got.UID != want.UID || got.IdentitySHA256 != want.IdentitySHA256 {
		t.Fatalf("identity fields lost: %+v", got)
	}
	if len(got.Models) != 2 || got.Factors["qwen3.8-flash"] != 0 || !got.RegionFacts["qwen3.8-flash"].IsFree {
		t.Fatalf("payload corrupted: %+v", got)
	}
	if tf := got.RegionFacts["gm51model"]; tf.PriceFactor == nil || *tf.PriceFactor != 1.5 || tf.ContextLength == nil || *tf.ContextLength != 200000 {
		t.Fatalf("pointer facts lost: %+v", tf)
	}
	if !got.FetchedAt.UTC().Truncate(time.Second).Equal(want.FetchedAt.UTC().Truncate(time.Second)) {
		t.Fatalf("fetched_at = %v, want %v", got.FetchedAt, want.FetchedAt)
	}
}

func TestModelCatalogRejectsInvalidDocuments(t *testing.T) {
	store := &qoderModelStore{root: t.TempDir()}
	cases := map[string]func(*modelCatalogCacheV1){
		"wrong hash": func(c *modelCatalogCacheV1) { c.IdentitySHA256 = modelCatalogIdentitySHA256(c.Region, "other-uid") },
		"bad region": func(c *modelCatalogCacheV1) {
			c.Region = "de"
			c.IdentitySHA256 = modelCatalogIdentitySHA256("de", c.UID)
		},
		"empty uid":   func(c *modelCatalogCacheV1) { c.UID = ""; c.IdentitySHA256 = modelCatalogIdentitySHA256(c.Region, "") },
		"no models":   func(c *modelCatalogCacheV1) { c.Models = nil },
		"model no id": func(c *modelCatalogCacheV1) { c.Models = []pluginapi.ModelInfo{{Name: "x"}} },
		"zero schema": func(c *modelCatalogCacheV1) {
			c.SchemaVersion = 0
			c.IdentitySHA256 = modelCatalogIdentitySHA256(c.Region, c.UID)
		},
		"future schema": func(c *modelCatalogCacheV1) {
			c.SchemaVersion = 99
			c.IdentitySHA256 = modelCatalogIdentitySHA256(c.Region, c.UID)
		},
	}
	for name, mutate := range cases {
		bad := catalogFixture(regionIntl, "uid-9", time.Now())
		mutate(&bad)
		if err := store.saveModelCatalog(bad); err == nil {
			t.Fatalf("%s: save should have failed", name)
		}
	}
	list, err := store.loadModelCatalogAll()
	if err != nil || len(list) != 0 {
		t.Fatalf("loadAll should see nothing: %d entries, err %v", len(list), err)
	}
}

// A future-schema file already on disk must abort an overwrite instead of
// being silently clobbered with data a newer build could no longer represent.
func TestModelCatalogAtomicWriteRefusesFutureSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, modelCatalogIdentitySHA256(regionCN, "uid-1")+".json")
	future := catalogFixture(regionCN, "uid-1", time.Now())
	future.SchemaVersion = modelCatalogSchemaVersion + 1
	futureRaw, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, futureRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	good := catalogFixture(regionCN, "uid-1", time.Now().Add(time.Hour))
	goodRaw, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	validate := func(cur []byte) error {
		var c modelCatalogCacheV1
		if err := json.Unmarshal(cur, &c); err != nil {
			return err
		}
		return validateModelCatalogCache(c)
	}
	err = writeModelCatalogAtomic(path, goodRaw, validate)
	if !errors.Is(err, errFutureModelCatalogSchema) {
		t.Fatalf("write over future schema = %v, want errFutureModelCatalogSchema", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(futureRaw) {
		t.Fatal("future-schema primary was modified")
	}
}

func TestModelCatalogBakFallback(t *testing.T) {
	store := &qoderModelStore{root: t.TempDir()}
	first := catalogFixture(regionCN, "uid-1", time.Now().Add(-2*time.Hour))
	if err := store.saveModelCatalog(first); err != nil {
		t.Fatal(err)
	}
	second := catalogFixture(regionCN, "uid-1", time.Now())
	second.Models = append(second.Models, pluginapi.ModelInfo{ID: "extra-model"})
	if err := store.saveModelCatalog(second); err != nil {
		t.Fatal(err)
	}
	// Corrupt only the primary; the .bak holds the first good document.
	path := filepath.Join(store.root, first.IdentitySHA256+".json")
	if err := os.WriteFile(path, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := store.loadModelCatalogAll()
	if err != nil || len(list) != 1 {
		t.Fatalf("loadAll after corruption: %d entries, err %v", len(list), err)
	}
	if len(list[0].Models) != len(first.Models) {
		t.Fatalf("fallback served wrong generation: %d models", len(list[0].Models))
	}
}

func TestSeedModelCatalogMemory(t *testing.T) {
	redirectUserConfigDir(t)
	store, err := defaultQoderModelStore()
	if err != nil {
		t.Fatal(err)
	}
	old := catalogFixture(regionIntl, "uid-i", time.Now().Add(-time.Hour))
	if err := store.saveModelCatalog(old); err != nil {
		t.Fatal(err)
	}
	freshCN := catalogFixture(regionCN, "uid-1", time.Now().Add(-time.Minute))
	if err := store.saveModelCatalog(freshCN); err != nil {
		t.Fatal(err)
	}

	// Swap the memory cache for a controlled one and restore afterwards.
	dynamicModelsCache.Lock()
	prev := dynamicModelsCache.byAccount
	dynamicModelsCache.byAccount = map[string]dynamicModelEntry{
		// A live entry newer than any file must survive seeding untouched.
		regionCN + "\x00uid-1": {models: []pluginapi.ModelInfo{{ID: "live-model"}}, fetched: time.Now()},
	}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byAccount = prev
		dynamicModelsCache.Unlock()
	})

	seedModelCatalogMemory()

	models, ok := cachedDynamicModelsFor(regionIntl + "\x00uid-i")
	if !ok || len(models) != 2 {
		t.Fatalf("intl account not seeded: ok=%v models=%d", ok, len(models))
	}
	facts, ok := cachedRegionFactsFor(regionIntl + "\x00uid-i")
	if !ok || !facts["qwen3.8-flash"].IsFree {
		t.Fatalf("region facts not seeded: ok=%v facts=%+v", ok, facts)
	}
	dynamicModelsCache.RLock()
	_, cnDup := dynamicModelsCache.byAccount[regionCN+"\x00uid-1"]
	cnEntry := dynamicModelsCache.byAccount[regionCN+"\x00uid-1"]
	dynamicModelsCache.RUnlock()
	if !cnDup || len(cnEntry.models) != 1 || cnEntry.models[0].ID != "live-model" {
		t.Fatalf("newer in-memory entry was overwritten by seeding: %+v", cnEntry)
	}
}

func TestPersistModelCatalogForHonoursUIDGate(t *testing.T) {
	redirectUserConfigDir(t)
	store, err := defaultQoderModelStore()
	if err != nil {
		t.Fatal(err)
	}
	sa := testStoredAuthCNAccount()

	persistModelCatalogFor(sa, []pluginapi.ModelInfo{{ID: "m1"}}, map[string]float64{"m1": 1}, nil)
	list, err := store.loadModelCatalogAll()
	if err != nil || len(list) != 1 {
		t.Fatalf("uid-bearing account not persisted: %d entries err %v", len(list), err)
	}
	if list[0].UID != "uid-1" || list[0].Region != regionCN {
		t.Fatalf("persisted identity wrong: %+v", list[0])
	}

	// UID-less (legacy token-prefix) accounts must never hit the disk store.
	legacy := &storedAuth{Account: storedAccount{Nickname: "legacy"}, Auth: storedTokens{Region: regionCN}}
	persistModelCatalogFor(legacy, []pluginapi.ModelInfo{{ID: "m2"}}, nil, nil)
	list, err = store.loadModelCatalogAll()
	if err != nil || len(list) != 1 {
		t.Fatalf("uid-less account must not persist: %d entries err %v", len(list), err)
	}
}
