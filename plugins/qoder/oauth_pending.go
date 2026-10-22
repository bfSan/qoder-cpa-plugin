package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	oauthPendingDir      = ".qoder-oauth-pending"
	oauthPendingMaxFiles = 128
	oauthPendingMaxBytes = 128 << 10
	oauthResultTTL       = 10 * time.Minute
	oauthOwnerHost       = "host"
	oauthOwnerPanel      = "panel"
)

// Only protocol material is restored. Endpoints and current proxy policy are
// always selected by compiled-in region and qoderRoutedHTTPDo, never disk data.
type oauthPendingRecord struct {
	Version   int                  `json:"version"`
	State     string               `json:"state"`
	Owner     string               `json:"owner"`
	Region    string               `json:"region"`
	Nonce     string               `json:"nonce"`
	Verifier  string               `json:"verifier"`
	DeviceKey string               `json:"device_key"`
	Created   time.Time            `json:"created"`
	Expires   time.Time            `json:"expires"`
	Token     *deviceTokenResponse `json:"token,omitempty"`
	JobToken  *jobTokenResponse    `json:"job_token,omitempty"`
	PAT       string               `json:"pat,omitempty"`
	TokenAt   time.Time            `json:"token_at,omitempty"`
	Result    *storedAuth          `json:"result,omitempty"`
	Saved     bool                 `json:"saved,omitempty"`
}

type oauthPendingStore struct {
	dir  string
	fd   int
	lock *os.File
	key  []byte
}

var oauthPending = struct {
	sync.Mutex
	store   *oauthPendingStore
	initErr error
}{}

var errOAuthStorage = errors.New("login persistence unavailable; retry this login without restarting the plugin")

func oauthPendingName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:]) + ".pending"
}

func oauthRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("login secure randomness unavailable")
	}
	return hex.EncodeToString(b), nil
}

func validOAuthState(state string) bool {
	if len(state) != 67 || !strings.HasPrefix(state, "qw-") {
		return false
	}
	_, err := hex.DecodeString(state[3:])
	return err == nil && strings.ToLower(state) == state
}

func validOAuthPendingName(name string) bool {
	if len(name) != 72 || !strings.HasSuffix(name, ".pending") {
		return false
	}
	_, err := hex.DecodeString(name[:64])
	return err == nil && strings.ToLower(name) == name
}

// Called by the parent on model/auth RPCs carrying Host. Lifecycle v7 requests
// have no Host. Empty config never writes HOME and does not unbind an existing
// store. A supplied bad directory fails only login; models can ignore the error.
func configureOAuthPending(raw []byte) (err error) {
	if len(raw) == 0 {
		return nil
	}
	var req struct {
		Host struct {
			AuthDir      string
			AuthDirSnake string `json:"auth_dir"`
		}
	}
	if json.Unmarshal(raw, &req) != nil {
		return errors.New("invalid login host configuration")
	}
	dir := strings.TrimSpace(req.Host.AuthDir)
	if dir == "" {
		dir = strings.TrimSpace(req.Host.AuthDirSnake)
	}
	if dir == "" {
		return nil
	}
	oauthPending.Lock()
	defer oauthPending.Unlock()
	defer func() { oauthPending.initErr = err }()
	if !filepath.IsAbs(dir) {
		return errors.New("login auth directory must be absolute")
	}
	dir = filepath.Join(filepath.Clean(dir), oauthPendingDir)
	if oauthPending.store != nil {
		if oauthPending.store.dir == dir {
			return nil
		}
		return errors.New("login auth directory changed; restart plugin to apply")
	}
	store, err := openOAuthPendingStore(dir)
	if err != nil {
		return err
	}
	oauthPending.store = store
	return nil
}

func validOAuthPendingRecord(r *oauthPendingRecord) bool {
	if r.Version != 1 || !validOAuthState(r.State) || (r.Owner != oauthOwnerHost && r.Owner != oauthOwnerPanel) || (r.Region != regionCN && r.Region != regionIntl) {
		return false
	}
	if len(r.Verifier) < 43 || len(r.Verifier) > 128 || strings.ContainsAny(r.Verifier, " \t\r\n") {
		return false
	}
	for _, c := range r.Verifier {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", c) {
			return false
		}
	}
	nonceLen := 36
	if r.Region == regionIntl {
		nonceLen = 32
	}
	if len(r.Nonce) != nonceLen || len(r.DeviceKey) != 36 {
		return false
	}
	if _, err := uuid.Parse(r.Nonce); err != nil {
		return false
	}
	if _, err := uuid.Parse(r.DeviceKey); err != nil {
		return false
	}
	now := time.Now()
	if r.Created.IsZero() || r.Created.After(now.Add(time.Minute)) || !r.Expires.After(r.Created) || r.Expires.After(r.Created.Add(loginTTL+oauthResultTTL)) {
		return false
	}
	token := r.Token != nil || r.JobToken != nil
	if r.Token != nil && r.JobToken != nil {
		return false
	}
	if token {
		if r.TokenAt.Before(r.Created) || r.TokenAt.After(now.Add(time.Minute)) || r.Expires.After(r.TokenAt.Add(oauthResultTTL)) || !r.Expires.After(r.TokenAt) {
			return false
		}
		if r.Token != nil && (r.Token.accessToken() == "" || r.Token.RefreshToken == "" || r.PAT != "") {
			return false
		}
		if r.JobToken != nil && (r.JobToken.Token == "" || r.JobToken.RefreshToken == "" || !strings.HasPrefix(r.PAT, "pt-") || r.Owner != oauthOwnerHost) {
			return false
		}
	} else if r.Result != nil || r.Saved || !r.TokenAt.IsZero() || r.PAT != "" || r.Expires.After(r.Created.Add(loginTTL)) {
		return false
	}
	if r.Result != nil {
		access, refresh, pat := "", "", ""
		if r.Token != nil {
			access, refresh = r.Token.accessToken(), r.Token.RefreshToken
		} else {
			access, refresh, pat = r.JobToken.Token, r.JobToken.RefreshToken, r.PAT
		}
		if r.Token != nil && r.Token.UserID != "" && r.Result.Account.UID != r.Token.UserID {
			return false
		}
		if sanitizeUIDForFileName(r.Result.Account.UID) == "" || r.Result.Auth.AccessToken != access || r.Result.Auth.RefreshToken != refresh || r.Result.Auth.PersonalToken != pat || r.Result.Auth.Region != r.Region || r.Result.Auth.Domain != domainForRegion(r.Region) {
			return false
		}
	}
	return !r.Saved || (r.Result != nil && r.Owner == oauthOwnerPanel)
}

func oauthRecord(state string, lc *loginCtx) *oauthPendingRecord {
	return &oauthPendingRecord{Version: 1, State: state, Owner: lc.owner, Region: lc.region, Nonce: lc.nonce, Verifier: lc.verifier, DeviceKey: lc.deviceKey, Created: time.Unix(0, lc.startedAt), Expires: lc.expires, Token: lc.token, JobToken: lc.jobToken, PAT: lc.pat, TokenAt: lc.tokenAt, Result: lc.result, Saved: lc.saved}
}

// lc.mu must be held. Memory is never rolled back on a failed disk write:
// subsequent polls retry this checkpoint, not the one-time upstream exchange.
func persistOAuthPending(state string, lc *loginCtx) error {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.initErr != nil {
		return oauthPending.initErr
	}
	if lc.store == nil {
		return nil
	}
	return lc.store.write(oauthRecord(state, lc))
}

func registerOAuthPending(state string, lc *loginCtx) error {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.initErr != nil {
		return oauthPending.initErr
	}
	count := 0
	loginStates.Range(func(_, _ any) bool { count++; return true })
	if count >= oauthPendingMaxFiles {
		return errors.New("too many pending logins; wait for expiry")
	}
	if _, ok := loginStates.Load(state); ok {
		return errors.New("duplicate login state")
	}
	lc.store = oauthPending.store
	if lc.store != nil {
		if _, err := lc.store.read(oauthPendingName(state)); !errors.Is(err, os.ErrNotExist) {
			return errOAuthStorage
		}
		if err := lc.store.write(oauthRecord(state, lc)); err != nil {
			return err
		}
	}
	loginStates.Store(state, lc)
	return nil
}

func lookupOAuthPending(state string) (*loginCtx, error) {
	if !validOAuthState(state) {
		return nil, errors.New("unknown or expired login state")
	}
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.initErr != nil {
		return nil, oauthPending.initErr
	}
	if v, ok := loginStates.Load(state); ok {
		return v.(*loginCtx), nil
	}
	if oauthPending.store != nil {
		rec, err := oauthPending.store.read(oauthPendingName(state))
		if err == nil && time.Now().Before(rec.Expires) {
			count := 0
			loginStates.Range(func(_, _ any) bool { count++; return true })
			if count >= oauthPendingMaxFiles {
				return nil, errors.New("too many pending logins; wait for expiry")
			}
			lc := &loginCtx{owner: rec.Owner, region: rec.Region, nonce: rec.Nonce, verifier: rec.Verifier, deviceKey: rec.DeviceKey, startedAt: rec.Created.UnixNano(), expires: rec.Expires, token: rec.Token, jobToken: rec.JobToken, pat: rec.PAT, tokenAt: rec.TokenAt, result: rec.Result, saved: rec.Saved, store: oauthPending.store}
			loginStates.Store(state, lc)
			return lc, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errOAuthStorage
		}
	}
	return nil, errors.New("unknown or expired login state")
}

func pruneOAuthPending(now time.Time) {
	loginStates.Range(func(k, v any) bool {
		lc := v.(*loginCtx)
		if lc.mu.TryLock() {
			if !now.Before(lc.expires) {
				oauthPending.Lock()
				loginStates.CompareAndDelete(k, lc)
				if lc.store != nil {
					lc.store.remove(oauthPendingName(k.(string)))
				}
				oauthPending.Unlock()
			}
			lc.mu.Unlock()
		}
		return true
	})
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.store != nil {
		_, _ = oauthPending.store.scan(now)
	}
}

// Owner binding prevents generic host RPC and panel from saving the same flow.
func lockOAuthPending(state, owner string) (*loginCtx, error) {
	lc, err := lookupOAuthPending(state)
	if err != nil {
		return nil, err
	}
	lc.mu.Lock()
	if lc.owner != owner {
		lc.mu.Unlock()
		return nil, errors.New("login belongs to another entry point")
	}
	if !time.Now().Before(lc.expires) {
		lc.mu.Unlock()
		return nil, errors.New("login expired; start a new login")
	}
	return lc, nil
}

// A token checkpoint always precedes auth construction/save. Result checkpoints
// replay until expiry; host RPC has no save acknowledgement and MUST NOT save.
func advanceOAuthPending(state string, lc *loginCtx, pat string) (bool, error) {
	if lc.token == nil && lc.jobToken == nil {
		// Reject corrupted/deleted disk state before consuming a one-time grant,
		// including when its in-memory context has not yet been evicted.
		if lc.store != nil {
			oauthPending.Lock()
			_, err := lc.store.read(oauthPendingName(state))
			oauthPending.Unlock()
			if err != nil {
				return false, errOAuthStorage
			}
		}
		if pat != "" {
			if !strings.HasPrefix(pat, "pt-") {
				return false, errors.New("PAT must start with pt-")
			}
			tok, err := exchangePATForJobToken(pat, lc.region)
			if err != nil {
				return false, err
			}
			lc.jobToken, lc.pat = tok, pat
		} else {
			tok, pending, err := pollDeviceToken(lc.nonce, lc.verifier, lc.region)
			if err != nil {
				return false, err
			}
			if pending || tok == nil {
				return false, nil
			}
			lc.token = tok
		}
		lc.tokenAt = time.Now()
		lc.expires = lc.tokenAt.Add(oauthResultTTL)
		if cap := time.Unix(0, lc.startedAt).Add(loginTTL + oauthResultTTL); lc.expires.After(cap) {
			lc.expires = cap
		}
	}
	if err := persistOAuthPending(state, lc); err != nil {
		return false, err
	}
	if lc.result == nil {
		var result *storedAuth
		if lc.token != nil {
			result = buildStoredAuthFromDeviceToken(lc.token, nil, lc.region)
			if lc.token.ExpiresIn > 0 {
				result.Auth.ExpiresAt = lc.tokenAt.Add(time.Duration(lc.token.ExpiresIn) * time.Millisecond).Unix()
			}
		} else {
			result = buildStoredAuthFromJobToken(lc.pat, lc.jobToken, nil, lc.region)
			if lc.jobToken.ExpiresIn > 0 {
				result.Auth.ExpiresAt = lc.tokenAt.Add(time.Duration(lc.jobToken.ExpiresIn) * time.Millisecond).Unix()
			}
		}
		// Qoder identity lookup is only needed for PAT/grants lacking user_id.
		// The token is already durable; lookup failure must not re-exchange it.
		// Never save a shared fallback filename over another account.
		if sanitizeUIDForFileName(result.Account.UID) == "" {
			ui, err := fetchUserInfo(result.Auth.AccessToken, lc.region)
			if err != nil || ui == nil || sanitizeUIDForFileName(ui.ID) == "" {
				return false, errors.New("login identity unavailable; retry this state")
			}
			result.Account.UID, result.Account.Nickname = ui.ID, ui.Name
		}
		lc.result = result
	}
	if err := persistOAuthPending(state, lc); err != nil {
		return false, err
	}
	return true, nil
}
