// model_store.go persists each account's fetched model catalog to disk so the
// panel (and model.static fan-out) can answer immediately after a CPA restart,
// instead of showing "not_loaded" until every account is re-discovered.
//
// This mirrors the workbuddy plugin's model_store.go (same operator directive:
// "目录缓存改掉，类似 workbuddy 落盘保存"), adapted to qoder's per-account cache
// shape: pluginapi.ModelInfo list + price factors + per-model capability facts.
//
// Guarantees, in the order they matter:
//   - Atomic replacement only: temp file in the target directory, fsync, then
//     rename, then directory fsync. A crash mid-write can never truncate a good
//     cache into a half-written one.
//   - A previous copy survives as <name>.json.bak, and reads fall back to it
//     when the primary decodes invalid. The .bak is only ever replaced with a
//     primary that itself validated.
//   - Reads validate identity (schema, region, UID, hash↔content match) and
//     reject a future schema version by skipping the file — never by deleting
//     it — so downgrades and roll-forwards lose no data.
//   - Writes are best-effort and silent: this plugin has no logger, a failing
//     cache write must never fail model discovery, which has already succeeded
//     upstream by that point.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelCatalogSchemaVersion is the only schema this build understands.
const modelCatalogSchemaVersion = 1

// Additive provenance marker: old files remain readable, but their inferred
// reasoning defaults and synthesized context fallback cannot become facts.
const modelCatalogFactsVersion = 1

var errFutureModelCatalogSchema = errors.New("future model catalog cache schema")

// modelCatalogCacheV1 is the on-disk document for one account's catalog.
//
// Region+UID are stored in clear text next to the hash so a load can verify
// the filename identity instead of trusting it: a file moved or renamed by
// hand decodes to a mismatch and is skipped, not adopted for the wrong account.
type modelCatalogCacheV1 struct {
	SchemaVersion  int                         `json:"schema_version"`
	FactsVersion   int                         `json:"facts_version,omitempty"`
	IdentitySHA256 string                      `json:"identity_sha256"`
	Region         string                      `json:"region"`
	UID            string                      `json:"uid"`
	FetchedAt      time.Time                   `json:"fetched_at"`
	Models         []pluginapi.ModelInfo       `json:"models"`
	Factors        map[string]float64          `json:"factors,omitempty"`
	RegionFacts    map[string]modelRegionFacts `json:"region_facts,omitempty"`
}

// qoderModelStore roots itself at one directory; all paths derive from it.
type qoderModelStore struct {
	root string
}

// defaultQoderModelStore uses the same CLIProxyAPI config-dir convention as
// workbuddy's store: <UserConfigDir>/CLIProxyAPI/qoder/model-catalog.
func defaultQoderModelStore() (*qoderModelStore, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &qoderModelStore{root: filepath.Join(configDir, "CLIProxyAPI", "qoder", "model-catalog")}, nil
}

// modelCatalogIdentitySHA256 hashes "<region>\x00<uid>", which is exactly the
// in-memory cache key shape (see accountCacheKey), so disk and memory keys are
// trivially interchangeable.
func modelCatalogIdentitySHA256(region, uid string) string {
	sum := sha256.Sum256([]byte(region + "\x00" + uid))
	return hex.EncodeToString(sum[:])
}

func validModelCatalogSHA(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateModelCatalogCache(cache modelCatalogCacheV1) error {
	if err := validateModelCatalogSchema(cache.SchemaVersion); err != nil {
		return err
	}
	switch strings.TrimSpace(cache.Region) {
	case regionCN, regionIntl:
	default:
		return fmt.Errorf("model catalog cache region %q invalid", cache.Region)
	}
	if strings.TrimSpace(cache.UID) == "" {
		return errors.New("model catalog cache uid is empty")
	}
	if !validModelCatalogSHA(cache.IdentitySHA256) {
		return errors.New("model catalog cache identity hash malformed")
	}
	if cache.IdentitySHA256 != modelCatalogIdentitySHA256(cache.Region, cache.UID) {
		return errors.New("model catalog cache identity hash mismatch")
	}
	if len(cache.Models) == 0 {
		return errors.New("model catalog cache has no models")
	}
	for _, m := range cache.Models {
		if strings.TrimSpace(m.ID) == "" {
			return errors.New("model catalog cache contains a model without an ID")
		}
	}
	return nil
}

func validateModelCatalogSchema(version int) error {
	switch {
	case version > modelCatalogSchemaVersion:
		return errFutureModelCatalogSchema
	case version == modelCatalogSchemaVersion:
		return nil
	default:
		return fmt.Errorf("model catalog cache schema %d unsupported", version)
	}
}

// saveModelCatalog writes one account document atomically (temp+fsync+rename,
// previous good copy demoted to .bak).
func (s *qoderModelStore) saveModelCatalog(cache modelCatalogCacheV1) error {
	if err := validateModelCatalogCache(cache); err != nil {
		return err
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, cache.IdentitySHA256+".json")
	return writeModelCatalogAtomic(path, raw, func(current []byte) error {
		var c modelCatalogCacheV1
		if err := json.Unmarshal(current, &c); err != nil {
			return err
		}
		return validateModelCatalogCache(c)
	})
}

// loadModelCatalogAll decodes every valid account document in the store.
// Primary files that fail to decode fall back to their .bak sibling; future
// schema files are skipped (kept on disk) rather than deleted.
func (s *qoderModelStore) loadModelCatalogAll() ([]modelCatalogCacheV1, error) {
	dirEntries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []modelCatalogCacheV1
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(de.Name(), ".json")
		if !validModelCatalogSHA(base) {
			continue
		}
		primary := filepath.Join(s.root, de.Name())
		if cache, ok := decodeModelCatalogFile(primary); ok {
			out = append(out, cache)
			continue
		}
		if cache, ok := decodeModelCatalogFile(primary + ".bak"); ok {
			out = append(out, cache)
		}
	}
	return out, nil
}

func decodeModelCatalogFile(path string) (modelCatalogCacheV1, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return modelCatalogCacheV1{}, false
	}
	var cache modelCatalogCacheV1
	if json.Unmarshal(raw, &cache) != nil {
		return modelCatalogCacheV1{}, false
	}
	if validateModelCatalogCache(cache) != nil {
		return modelCatalogCacheV1{}, false
	}
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".bak"), ".json")
	if name != cache.IdentitySHA256 {
		return modelCatalogCacheV1{}, false
	}
	return cache, true
}

// persistModelCatalogFor writes a freshly fetched account catalog to disk.
// Best-effort by design: the models are already cached in memory at this
// point, and a failing disk write must never surface as a discovery error.
//
// Only UID-bearing accounts are persisted. Token-prefix keys (legacy files
// predating UID capture) rotate with credentials and would litter the store
// with entries nothing can look up after re-login.
// existingModelCatalogFacts returns the cached factors/facts for one account so a
// catalog-only refresh can preserve capability data it cannot itself re-derive.
func existingModelCatalogFacts(sa *storedAuth) (map[string]float64, map[string]modelRegionFacts) {
	if sa == nil {
		return nil, nil
	}
	uid := strings.TrimSpace(sa.Account.UID)
	if uid == "" {
		return nil, nil
	}
	store, err := defaultQoderModelStore()
	if err != nil {
		return nil, nil
	}
	region := authRegion(sa)
	caches, err := store.loadModelCatalogAll()
	if err != nil {
		return nil, nil
	}
	identity := modelCatalogIdentitySHA256(region, uid)
	for _, cache := range caches {
		if cache.Region == region && cache.IdentitySHA256 == identity {
			return cache.Factors, cache.RegionFacts
		}
	}
	return nil, nil
}

func persistModelCatalogFor(sa *storedAuth, models []pluginapi.ModelInfo, factors map[string]float64, facts map[string]modelRegionFacts) {
	if sa == nil || len(models) == 0 {
		return
	}
	uid := strings.TrimSpace(sa.Account.UID)
	if uid == "" {
		return
	}
	store, err := defaultQoderModelStore()
	if err != nil {
		return
	}
	region := authRegion(sa)
	_ = store.saveModelCatalog(modelCatalogCacheV1{
		SchemaVersion:  modelCatalogSchemaVersion,
		FactsVersion:   modelCatalogFactsVersion,
		IdentitySHA256: modelCatalogIdentitySHA256(region, uid),
		Region:         region,
		UID:            uid,
		FetchedAt:      time.Now(),
		Models:         models,
		Factors:        factors,
		RegionFacts:    facts,
	})
}

// seedModelCatalogMemory loads the disk store into the in-memory account
// cache at plugin registration, so the first model.static / region_status
// after a CPA restart answers from disk instead of "not loaded".
//
// Seeded entries keep their original FetchedAt: the 12h TTL gate still
// decides when an upstream re-pull is due, so disk seeding short-circuits
// fan-out for same-day restarts and ages out honestly for older ones. An
// in-memory entry is only ever replaced by a strictly newer disk document
// (a live fetch result is fresher than any file and must not lose to it).
func seedModelCatalogMemory() {
	store, err := defaultQoderModelStore()
	if err != nil {
		return
	}
	caches, err := store.loadModelCatalogAll()
	if err != nil || len(caches) == 0 {
		return
	}
	now := time.Now()
	dynamicModelsCache.Lock()
	defer dynamicModelsCache.Unlock()
	if dynamicModelsCache.byAccount == nil {
		dynamicModelsCache.byAccount = make(map[string]dynamicModelEntry)
	}
	for _, c := range caches {
		key := c.Region + "\x00" + c.UID
		if c.FetchedAt.After(now) {
			// A clock-skewed file could otherwise never expire; clamp to now
			// so the TTL gate still re-fetches it on the next discovery.
			c.FetchedAt = now
		}
		if cur, ok := dynamicModelsCache.byAccount[key]; ok && !c.FetchedAt.After(cur.fetched) {
			continue
		}
		dynamicModelsCache.byAccount[key] = dynamicModelEntry{
			models:  c.Models,
			factors: c.Factors,
			region:  trustedLoadedCatalogFacts(c),
			fetched: c.FetchedAt,
		}
	}
}

// writeModelCatalogAtomic implements the temp+fsync+rename+dir-fsync dance,
// keeping the previous validated primary as <path>.bak. A future-schema file
// at either location aborts the write instead of being overwritten — that is
// the one error worth surfacing to the (silent) caller.
func writeModelCatalogAtomic(path string, data []byte, validateCurrent func([]byte) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tempPath, err := writeSyncedCatalogTemp(dir, filepath.Base(path), data)
	if err != nil {
		return err
	}
	defer os.Remove(tempPath)

	current, currentExists, err := readCatalogIfExists(path)
	if err != nil {
		return err
	}
	backup, backupExists, err := readCatalogIfExists(path + ".bak")
	if err != nil {
		return err
	}
	if currentExists && errors.Is(validateCurrent(current), errFutureModelCatalogSchema) {
		return errFutureModelCatalogSchema
	}
	if backupExists && errors.Is(validateCurrent(backup), errFutureModelCatalogSchema) {
		return errFutureModelCatalogSchema
	}
	if currentExists && validateCurrent(current) == nil {
		bakTemp, err := writeSyncedCatalogTemp(dir, filepath.Base(path)+".bak", current)
		if err != nil {
			return err
		}
		defer os.Remove(bakTemp)
		if err := replaceCatalogFile(bakTemp, path+".bak"); err != nil {
			return err
		}
	}
	if err := replaceCatalogFile(tempPath, path); err != nil {
		return err
	}
	return syncCatalogDir(dir)
}

func readCatalogIfExists(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func writeSyncedCatalogTemp(dir, base string, data []byte) (string, error) {
	file, err := os.CreateTemp(dir, "."+base+".tmp-")
	if err != nil {
		return "", err
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	n, err := file.Write(data)
	if err != nil {
		return "", err
	}
	if n != len(data) {
		return "", fmt.Errorf("write catalog temp: wrote %d of %d bytes", n, len(data))
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func replaceCatalogFile(temp, destination string) error {
	if runtime.GOOS == "windows" {
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(temp, destination)
}

func syncCatalogDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// Upgrade old cache evidence conservatively without deleting it or breaking
// registration. Previous builds guessed strongest effort/default input values,
// and did not preserve whether is_reasoning was absent versus explicitly false.
func trustedLoadedCatalogFacts(cache modelCatalogCacheV1) map[string]modelRegionFacts {
	out := make(map[string]modelRegionFacts, len(cache.RegionFacts))
	for id, raw := range cache.RegionFacts {
		fact := cloneRegionFacts(raw)
		if cache.FactsVersion != modelCatalogFactsVersion {
			if len(fact.ContextTiers) == 0 {
				fact.ContextLength = nil
			}
			if fact.Thinking != nil {
				fact.Thinking.Default = ""
				if fact.Thinking.Status == "unsupported" {
					fact.Thinking.Status = "unknown"
				}
			}
		}
		out[id] = fact
	}
	return out
}
