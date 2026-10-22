// oauth.go implements Qoder DeviceFlow/PKCE login (dt-/drt-), legacy
// PAT exchange (jt-/jrt-), and credential refresh. Pending login state and
// token/result checkpoints live in oauth_pending*.go. Host RPC returns Auth for
// the host to save; the management entry saves its own owner-bound state once.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// jobTokenResponse mirrors the upstream /api/v1/jobToken/{exchange,refresh}
// response payload.
type jobTokenResponse struct {
	Token                 string `json:"token"`         // jt-..., 24h
	RefreshToken          string `json:"refresh_token"` // jrt-..., 48h
	ExpiresAt             string `json:"expires_at"`    // RFC3339
	ExpiresIn             int64  `json:"expires_in"`    // ms
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"` // ms
}

// Test seams retain production routing on every request, not a login-time client.
var oauthHTTPDo = qoderRoutedHTTPDo
var oauthSaveAuthJSON = hostAuthSaveJSON

func oauthRoutedHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	resp, err := oauthHTTPDo(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("oauth transport unavailable; retry this login")
	}
	if resp == nil || len(resp.Body) > oauthPendingMaxBytes {
		return nil, fmt.Errorf("oauth invalid or oversized response")
	}
	return resp, nil
}

// doRawJSON sends method to fullURL with the given headers and returns the
// raw response body. The legacy client argument is ignored: current routing
// policy is consulted on every request. Used for plain JSON (no envelope)
// like /api/v1/jobToken/exchange and /api/v1/jobToken/refresh.
func doRawJSON(_ *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := oauthRoutedHTTPDo(req)
	if err != nil {
		return nil, 0, err
	}
	raw := resp.Body
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}

// exchangePATForJobToken calls POST /api/v1/jobToken/exchange with a PAT and
// returns the resulting jt-/jrt- pair.
func exchangePATForJobToken(pat, region string) (*jobTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	data, _, err := doRawJSON(nil, http.MethodPost, upstreamBaseForRegion(region)+"/api/v1/jobToken/exchange", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out jobTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("jobToken exchange parse: %w", err)
	}
	if out.Token == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("jobToken exchange: empty token pair in response")
	}
	return &out, nil
}

// refreshJobToken calls POST /api/v1/jobToken/refresh with a jrt-.
func refreshJobToken(jrt, region string) (*jobTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": jrt})
	data, _, err := doRawJSON(nil, http.MethodPost, upstreamBaseForRegion(region)+"/api/v1/jobToken/refresh", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out jobTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("jobToken refresh parse: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("jobToken refresh: empty token in response")
	}
	return &out, nil
}

// userInfoResponse is the minimal subset of GET /api/v1/userinfo we need to
// populate auth identity (uid, nickname).
// Upstream returns: {"id":"...","name":"aliyun...","username":"...",
// "organization_id":"","organization_name":"",...}
type userInfoResponse struct {
	ID               string `json:"id"`       // uuid — used as our UID
	Name             string `json:"name"`     // display name
	Username         string `json:"username"` // alternate uuid
	Avatar           string `json:"avatar"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	UserType         string `json:"user_type"` // may be absent; falls back to "personal_professional_trial"
}

// fetchUserInfo queries /api/v1/userinfo with a jt- Bearer to populate the
// auth's identity fields (uid, nickname, user_type for COSY signing).
// The endpoint returns plain JSON (no envelope) — same as jobToken/exchange.
func fetchUserInfo(jt, region string) (*userInfoResponse, error) {
	req, err := http.NewRequest(http.MethodGet, endpointUserInfoForRegion(region), nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jt)
	resp, err := oauthRoutedHTTPDo(req)
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo: http %d", resp.StatusCode)
	}
	var out userInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("userinfo: invalid response")
	}
	return &out, nil
}

// buildStoredAuthFromJobToken constructs a storedAuth from a PAT + jobToken
// pair + user identity. Called by both PAT import and OAuth-like poll.
func buildStoredAuthFromJobToken(pat string, tok *jobTokenResponse, ui *userInfoResponse, region string) *storedAuth {
	expiresAt := time.Now().Add(24 * time.Hour).Unix()
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Millisecond).Unix()
	}

	nickname := ""
	uid := ""
	if ui != nil {
		uid = ui.ID
		nickname = ui.Name
	}

	// PersonalToken is stored inside RefreshToken only when refresh is via PAT
	// (no jrt available). For QoderWork we have a real jrt- (refresh token),
	// so RefreshToken carries the jrt- and the PAT itself is kept in Domain
	// alongside the realm for later re-exchange when jrt- expires (48h).
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:   tok.Token,
			RefreshToken:  tok.RefreshToken,
			PersonalToken: pat, // PAT stored for automatic re-exchange when jrt- expires (48h)
			ExpiresAt:     expiresAt,
			Domain:        domainForRegion(region),
			Region:        region,
		},
		Account: storedAccount{
			UID:      uid,
			Nickname: nickname,
		},
	}
}

func uiUserType(ui *userInfoResponse) string {
	if ui == nil || ui.UserType == "" {
		return "personal_professional_trial"
	}
	return ui.UserType
}

// -----------------------------------------------------------------------------
// Device-authorization login (real OAuth — no PAT required)
//
// Reverse-engineered from the official QoderWork CN desktop client
// (/tmp/qw_extract .../main.js) and proven live against the Global realm by
// /root/qoder-register/qoder_device_oauth.py (2026-07-22, issued dt-/drt-
// tokens). CN realm constants from the same main.js:
//
//	WEBSITE_DOMAIN  = qoder.com.cn        (auth pages)
//	OPENAPI_DOMAIN  = openapi.qoder.com.cn (token endpoints)
//	CLIENT_ID prod  = 1c5e33e1-364d-4ce6-b02c-acaa81274a5c (shared with Global)
//	REDIRECT_URI    = qoder-work-cn://
//
// Flow: StartLogin builds the /device/selectAccounts URL with a PKCE
// challenge; the user authorizes in their browser; PollLogin polls
// /api/v1/deviceToken/poll with the verifier until the grant lands
// (404/202 = pending). Tokens: dt- (~30d) + drt- refresh (~1y), refreshed via
// POST /api/v1/deviceToken/refresh — no PAT involved anywhere.
// -----------------------------------------------------------------------------

const (
	qoderWebsiteCN     = "https://qoder.com.cn"
	qoderClientIDCN    = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	qoderRedirectURICN = "qoder-work-cn://"
	// Intl device-authorization entry (merged qoder-intl plugin, v0.10.0).
	qoderWebsiteIntl     = "https://qoder.com"
	qoderClientIDIntl    = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	qoderRedirectURIIntl = "qoder://aicoding.aicoding-agent/login-success"
)

// deviceTokenResponse mirrors /api/v1/deviceToken/{poll,refresh} payloads.
// expires_in / refresh_token_expires_in are MILLISECONDS (client main.js and
// the live Global sample agree).
type deviceTokenResponse struct {
	Token                 string `json:"token"`
	DeviceToken           string `json:"device_token"` // refresh path returns this key instead
	RefreshToken          string `json:"refresh_token"`
	UserID                string `json:"user_id"`
	ExpiresAt             string `json:"expires_at"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

func (d *deviceTokenResponse) accessToken() string {
	if d.Token != "" {
		return d.Token
	}
	return d.DeviceToken
}

// makePKCE returns (verifier, challenge) per RFC 7636 (S256).
func makePKCE() (string, string) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", ""
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// handleStartLogin implements AuthProvider.StartLogin: build the device
// authorization URL and stash the PKCE verifier under the returned state.
//
// The effective region is, in order:
//  1. an explicit region=cn|intl query parameter supplied by the caller,
//  2. the plugin's login_region configuration,
//  3. cn.
//
// The host forwards the query string of /v0/management/qoder-auth-url as
// request metadata, which is how the Qoder panel exposes separate CN and Intl
// login actions without forking the plugin into two OAuth entries.
func handleStartLogin(raw []byte) ([]byte, error) {
	region := loadedLoginRegion()
	var req pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		if requested := requestedLoginRegion(req.Metadata); requested != "" {
			region = requested
		}
	}
	return startLoginWithRegion(raw, region)
}

// handleManagementOAuthStart starts a device-flow login for the panel without
// going through the host's generic /v0/management/qoder-auth-url route.
//
// The host's ServePluginAuthURL does not forward the HTTP query string into
// StartLogin's Metadata, so a caller cannot reliably select CN vs Intl there.
// The panel therefore calls this management route, which reads region from the
// request body/query and stores it in the plugin-side login state.
func handleManagementOAuthStart(req pluginapi.ManagementRequest) map[string]any {
	region := regionCN
	if raw := strings.TrimSpace(req.Query.Get("region")); raw != "" {
		region = normalizeRegion(raw)
	}
	if len(req.Body) > 0 {
		var body struct {
			Region string `json:"region"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil && strings.TrimSpace(body.Region) != "" {
			region = normalizeRegion(body.Region)
		}
	}
	raw, err := startLoginOwned(nil, region, oauthOwnerPanel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		msg := "failed to start login"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return map[string]any{"error": msg}
	}
	var start pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &start); err != nil {
		return map[string]any{"error": "failed to decode login response"}
	}
	loginURL, err := url.Parse(start.URL)
	if err != nil {
		return map[string]any{"error": "failed to parse login URL"}
	}
	params := make(map[string]string, len(loginURL.Query()))
	for key, values := range loginURL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}
	urlBase := *loginURL
	urlBase.RawQuery = ""
	urlBase.Fragment = ""
	return map[string]any{
		"url":        start.URL,
		"url_base":   urlBase.String(),
		"query":      params,
		"state":      start.State,
		"region":     region,
		"expires_at": start.ExpiresAt,
		"persistent": start.Metadata["persistent"],
		"save_owner": oauthOwnerPanel,
		"resume":     "poll the same state until expires_at; token and result checkpoints survive restart when persistent=true",
	}
}

// handleManagementOAuthPoll polls one plugin-owned device login and persists
// the credential through host.auth.save when the browser grant lands.
func handleManagementOAuthPoll(req pluginapi.ManagementRequest) map[string]any {
	state := strings.TrimSpace(req.Query.Get("state"))
	if state == "" && len(req.Body) > 0 {
		var body struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil {
			state = strings.TrimSpace(body.State)
		}
	}
	if state == "" {
		return map[string]any{"status": "error", "error": "state is required"}
	}
	lc, err := lockOAuthPending(state, oauthOwnerPanel)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error(), "state": state}
	}
	defer lc.mu.Unlock()
	response := func(status string) map[string]any {
		return map[string]any{"status": status, "state": state, "region": lc.region, "expires_at": lc.expires.UTC(), "persistent": lc.store != nil, "save_owner": oauthOwnerPanel}
	}
	ready, err := advanceOAuthPending(state, lc, "")
	if err != nil {
		res := response("error")
		res["error"] = err.Error()
		res["retryable"] = true
		return res
	}
	if !ready {
		return response("wait")
	}
	if !lc.saved {
		sa := lc.result
		fileJSON, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
		if err != nil {
			res := response("error")
			res["error"] = "login credential preparation failed; retry this state"
			res["retryable"] = true
			return res
		}
		if err := oauthSaveAuthJSON(authFileNameFor(sa), fileJSON); err != nil {
			res := response("error")
			res["error"] = "login credential save failed; retry this state"
			res["retryable"] = true
			return res
		}
		lc.saved = true
	}
	// Even when the saved-marker checkpoint fails, keep saved=true in memory.
	// After a crash in that tiny window the deterministic filename makes the
	// host save an idempotent overwrite (exactly-once RPC is not available).
	if err := persistOAuthPending(state, lc); err != nil {
		res := response("error")
		res["error"] = err.Error()
		res["retryable"] = true
		return res
	}
	res := response("ok")
	res["uid"], res["nickname"], res["file"] = lc.result.Account.UID, lc.result.Account.Nickname, authFileNameFor(lc.result)
	return res
}

// requestedLoginRegion extracts region=cn|intl from login metadata. It accepts
// string and []string values because the host may preserve repeated query
// parameters. An unknown or empty value is ignored so login_region remains
// the compatibility default.
func requestedLoginRegion(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata["region"]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return explicitLoginRegion(v)
	case []string:
		if len(v) > 0 {
			return explicitLoginRegion(v[0])
		}
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return explicitLoginRegion(s)
			}
		}
	}
	return ""
}

func explicitLoginRegion(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case regionIntl, "global":
		return regionIntl
	case regionCN:
		return regionCN
	default:
		return ""
	}
}

// startLoginWithRegion starts a device-flow login pinned to region.
func startLoginWithRegion(raw []byte, region string) ([]byte, error) {
	return startLoginOwned(raw, region, oauthOwnerHost)
}

func startLoginOwned(raw []byte, region, owner string) ([]byte, error) {
	if err := configureOAuthPending(raw); err != nil {
		return nil, err
	}
	region = normalizeRegion(region)
	verifier, challenge := makePKCE()
	if verifier == "" {
		return nil, fmt.Errorf("login secure randomness unavailable")
	}
	// nonce: intl protocol (cockpit-tools v1.3.36, aligned 2026-09-02)
	// uses 32-hex uuid-simple; CN (cpa-plugin qoderwork reference) keeps
	// the dashed uuid.
	nonceID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("login secure randomness unavailable")
	}
	deviceID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("login secure randomness unavailable")
	}
	nonce := nonceID.String()
	if region == regionIntl {
		nonce = strings.ReplaceAll(nonce, "-", "")
	}
	machineID := deviceID.String()

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("redirect_uri", qoderRedirectURIFor(region))
	if region != regionIntl {
		// CN (qoder.com.cn, qoderwork protocol): client_id + machine_id stay.
		q.Set("client_id", qoderClientIDFor(region))
		// intl (qoder.com): cockpit-tools v1.3.36 dropped client_id and
		// machine_id from the device-login URL — the grant API now rejects
		// the legacy pair with "Parameter invalid" after the user
		// authorizes. Only nonce/challenge/challenge_method/redirect_uri
		// are sent. CN keeps the qoderwork protocol.
		q.Set("machine_id", machineID)
	}
	authURL := qoderWebsiteFor(region) + "/device/selectAccounts?" + q.Encode()

	now := time.Now()
	stateID, err := oauthRandomHex(32)
	if err != nil {
		return nil, err
	}
	state := "qw-" + stateID
	lc := &loginCtx{verifier: verifier, nonce: nonce, deviceKey: machineID, region: region, owner: owner, expires: now.Add(loginTTL), startedAt: now.UnixNano()}
	if err := registerOAuthPending(state, lc); err != nil {
		return nil, err
	}
	response := pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       authURL,
		State:     state,
		ExpiresAt: now.Add(loginTTL).UTC(),
		Metadata: map[string]any{
			"persistent": lc.store != nil,
			"save_owner": owner,
			"resume":     "poll the same state; token/result checkpoints are replayable until expires_at",
			"logo":       pluginLogoURL,
			"region":     region,
			"prompt":     "在打开的页面中登录并授权 Qoder（设备授权，无需 PAT）。完成后此窗口会自动关闭。",
		},
	}
	return okEnvelope(struct {
		pluginapi.AuthLoginStartResponse
		Persistent bool   `json:"persistent"`
		SaveOwner  string `json:"save_owner"`
	}{response, lc.store != nil, owner})
}

// pollDeviceToken performs one GET against /api/v1/deviceToken/poll.
// Returns (tok, pending, error): pending=true means the user hasn't finished
// authorizing yet (upstream 404/202) — the host should keep polling.
func pollDeviceToken(nonce, verifier, region string) (*deviceTokenResponse, bool, error) {
	q := url.Values{}
	q.Set("nonce", nonce)
	q.Set("verifier", verifier)
	q.Set("challenge_method", "S256")
	fullURL := upstreamBaseForRegion(region) + "/api/v1/deviceToken/poll?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	resp, err := oauthRoutedHTTPDo(req)
	if err != nil {
		return nil, false, err
	}
	raw := resp.Body
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, true, nil
	}
	if resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("deviceToken poll: http %d", resp.StatusCode)
	}
	var out deviceTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("deviceToken poll: invalid response")
	}
	if out.accessToken() == "" {
		return nil, true, nil // 200 without token yet — treat as pending
	}
	if out.RefreshToken == "" {
		return nil, false, fmt.Errorf("deviceToken poll: incomplete token pair")
	}
	return &out, false, nil
}

// refreshDeviceToken calls POST /api/v1/deviceToken/refresh with a drt-.
func refreshDeviceToken(drt, region string) (*deviceTokenResponse, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": drt})
	data, _, err := doRawJSON(nil, http.MethodPost, upstreamBaseForRegion(region)+"/api/v1/deviceToken/refresh", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out deviceTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	if out.accessToken() == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("deviceToken refresh: incomplete token pair in response")
	}
	return &out, nil
}

// handlePollLogin returns a replayable auth result; the host alone saves it.
// Never infer completion by scanning other accounts: that crosses sessions.
func handlePollLogin(raw []byte) ([]byte, error) {
	if err := configureOAuthPending(raw); err != nil {
		return nil, err
	}
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("invalid login poll request")
	}
	state := strings.TrimSpace(req.State)
	lc, err := lockOAuthPending(state, oauthOwnerHost)
	if err != nil {
		return nil, err
	}
	defer lc.mu.Unlock()
	pat, _ := req.Metadata["pat"].(string)
	ready, err := advanceOAuthPending(state, lc, strings.TrimSpace(pat))
	if err != nil {
		return nil, err
	}
	response := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "等待浏览器完成 QoderWork 设备授权"}
	if ready {
		response.Status = pluginapi.AuthLoginStatusSuccess
		response.Auth = toAuthData(lc.result)
		response.Message = ""
	}
	// Embed the SDK response to preserve wire compatibility while providing
	// explicit resume semantics for callers that retain state across restarts.
	return okEnvelope(struct {
		pluginapi.AuthLoginPollResponse
		State      string    `json:"state"`
		ExpiresAt  time.Time `json:"expires_at"`
		Persistent bool      `json:"persistent"`
		SaveOwner  string    `json:"save_owner"`
	}{response, state, lc.expires.UTC(), lc.store != nil, oauthOwnerHost})
}

// deviceExpiryUnix resolves the access-token expiry from a deviceToken
// poll/refresh response. Poll returns expires_in (ms); refresh returns only
// expires_at (RFC3339). Default: 30 days (observed dt- lifetime).
func deviceExpiryUnix(tok *deviceTokenResponse) int64 {
	if tok.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(tok.ExpiresIn) * time.Millisecond).Unix()
	}
	if tok.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil {
			return t.Unix()
		}
	}
	return time.Now().Add(30 * 24 * time.Hour).Unix()
}

// buildStoredAuthFromDeviceToken maps a device-token grant onto storedAuth.
// AccessToken=dt-, RefreshToken=drt-. When ui is nil (fast path during
// PollLogin), UserID comes from the poll response and nickname is empty —
// the panel fills it lazily on first load. preservePAT=false skips the
// hostAuthList scan to avoid blocking auth-file persistence.
func buildStoredAuthFromDeviceToken(tok *deviceTokenResponse, ui *userInfoResponse, region string) *storedAuth {
	expiresAt := deviceExpiryUnix(tok)
	uid := tok.UserID
	nickname := ""
	if ui != nil {
		if ui.ID != "" {
			uid = ui.ID
		}
		nickname = ui.Name
	}
	// When ui is nil (fast PollLogin path), derive a readable nickname from
	// the uid so the panel doesn't fall back to the raw filename label.
	if nickname == "" {
		if len(uid) > 8 {
			nickname = "u" + uid[len(uid)-8:]
		} else {
			nickname = "u" + uid
		}
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:   tok.accessToken(),
			RefreshToken:  tok.RefreshToken,
			PersonalToken: "", // PAT coalescing happens on keepalive, not here
			ExpiresAt:     expiresAt,
			Domain:        domainForRegion(region),
			Region:        region,
		},
		Account: storedAccount{UID: uid, Nickname: nickname},
	}
}

// existingPATForUID looks up an existing qoderwork auth file for the same
// uid and returns its stored PAT (empty when none). Lets OAuth re-login
// preserve a previously imported PAT instead of wiping it.
//
// NOTE: This calls hostAuthList which is a blocking host RPC. It must NOT
// be called during PollLogin (delays auth file persistence). It is safe to
// call during keepalive/refresh which happens long after the file is written.
func existingPATForUID(uid string) string {
	if uid == "" {
		return ""
	}
	files, err := hostAuthList()
	if err != nil {
		return ""
	}
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), providerName+"-") {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil || sa == nil {
			continue
		}
		if sa.Account.UID == uid && strings.HasPrefix(sa.Auth.PersonalToken, "pt-") {
			return sa.Auth.PersonalToken
		}
	}
	return ""
}

// handleRefreshAuth implements AuthProvider.Refresh. The two credential
// families coexist in one auth file and are routed by TOKEN PREFIX, not by
// the presence of a PAT — an OAuth account may ALSO carry a PAT for fallback:
//
//  1. OAuth device family (refresh token starts with "drt-"): POST
//     /api/v1/deviceToken/refresh — the device flow's own endpoint.
//     The PAT (if any) is preserved untouched in the file.
//  2. Legacy PAT family (refresh token starts with "jrt-", or a bare PAT):
//     jrt- refresh → PAT re-exchange, as before.
//
// The previous PersonalToken-presence routing had a fatal flaw: once a PAT
// coexisted, a device account was force-refreshed through the jobToken
// endpoints, which reject drt- tokens, and the PAT fallback then OVERWROTE
// the OAuth credential with a jobToken pair — destroying the OAuth session.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}

	if strings.HasPrefix(sa.Auth.RefreshToken, "drt-") {
		// OAuth device family — deviceToken/refresh ONLY.
		tok, err := refreshDeviceToken(sa.Auth.RefreshToken, authRegion(sa))
		if err != nil {
			return nil, fmt.Errorf("refresh rejected: %w — deviceToken refresh failed; re-login via OAuth required", err)
		}
		sa.Auth.AccessToken = tok.accessToken()
		sa.Auth.RefreshToken = tok.RefreshToken
		sa.Auth.ExpiresAt = preserveExpiry(deviceExpiryUnix(tok), sa.Auth.ExpiresAt)
		invalidateCosySession(sa.Account.UID)
		return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
	}

	// Legacy PAT family: Tier 1 jrt- refresh → Tier 2 PAT re-exchange.
	tok, err := refreshJobToken(sa.Auth.RefreshToken, authRegion(sa))
	if err != nil && sa.Auth.PersonalToken != "" {
		// Tier 2: jrt- expired — fall back to PAT re-exchange.
		tok, err = exchangePATForJobToken(sa.Auth.PersonalToken, authRegion(sa))
	}
	if err != nil {
		return nil, fmt.Errorf("refresh rejected: %w — both jrt- refresh and PAT re-exchange failed; re-import PAT required", err)
	}
	sa.Auth.AccessToken = tok.Token
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	sa.Auth.ExpiresAt = preserveExpiry(
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Millisecond).Unix(),
		sa.Auth.ExpiresAt,
	)
	invalidateCosySession(sa.Account.UID)
	// Host persists the refreshed credential itself after Refresh returns
	// (conductor.go refreshAuth → m.Update → persist). Writing from the
	// plugin too would double-write the file.
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn. Zero would tell the host the credential is
// permanently expired and trigger a refresh storm on every request.
// preserveExpiry keeps the previous value when the refresh did not yield a new
// expiry. Both sides are normalised to seconds: the stored field is seconds by
// contract, but callers historically produced millisecond values, which read
// back as 1970 and made every token look long expired.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if normalized := tokenExpiryUnix(newExpiry); normalized > 0 {
		return normalized
	}
	return tokenExpiryUnix(oldExpiry)
}

// toAuthDataForRefresh mirrors the workbuddy helper: blank out FileName and
// ID so the host backfills from the original auth path (prevents ID mismatch
// duplicate files when Refresh round-trips the record).
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
