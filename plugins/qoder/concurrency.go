// concurrency.go implements the per-account upstream concurrency gate.
//
// Why this exists: the plugin had no in-flight limit at all, so a burst of
// client requests on one credential fanned out to the gateway simultaneously.
// The upstream counts concurrent requests per uid and starts answering with a
// 429 once roughly 15–16 are in flight, which is exactly the state the envelope
// status fix (stream.go) now reports honestly. Serialising a little at the
// source prevents entering that penalty window instead of repeatedly backing
// off out of it.
//
// The gate is keyed by credential: one semaphore per authID/uid, so a busy
// account cannot starve a second account, and each account gets the full
// allowance.
package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const (
	// upstreamMaxConcurrentPerAuth caps in-flight upstream requests per
	// credential. The observed upstream threshold is ~15–16 simultaneous
	// requests per uid; 8 keeps a wide margin so normal bursts never trip it.
	upstreamMaxConcurrentPerAuth = 8

	// upstreamGateAcquireTimeout bounds how long a request waits for a slot.
	// Waiting forever would hide the saturation from the caller; failing fast
	// gives CPA a retryable error it can route to another credential.
	upstreamGateAcquireTimeout = 30 * time.Second
)

// errUpstreamGateTimeout is returned when no concurrency slot became available
// within upstreamGateAcquireTimeout. It deliberately carries no HTTP status: a
// saturated local gate is not an upstream credential fault, so the host must
// not cool the auth for it.
var errUpstreamGateTimeout = errors.New("qoder upstream concurrency gate: timed out waiting for a free slot (unexpected EOF)")

type upstreamGate struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

var upstreamGateRegistry = &upstreamGate{slots: map[string]chan struct{}{}}

// gateKey normalizes the credential identity used for throttling. AuthID is the
// host's auth file/index handle and is present on every executor request; the
// uid is a stable fallback for paths that only know the account.
func gateKey(authID, authUID string) string {
	if key := strings.TrimSpace(authID); key != "" {
		return key
	}
	return strings.TrimSpace(authUID)
}

// channelFor returns (creating on demand) the semaphore channel for one key.
func (g *upstreamGate) channelFor(key string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.slots[key]
	if !ok {
		ch = make(chan struct{}, upstreamMaxConcurrentPerAuth)
		g.slots[key] = ch
	}
	return ch
}

// acquire takes one slot for the credential, waiting at most timeout.
//
// The returned release function is safe to call more than once and safe on a
// nil receiver, so callers can `defer release()` without tracking state: a
// double release would otherwise hand back a slot twice and let the gate drift
// past its cap.
func (g *upstreamGate) acquire(key string, timeout time.Duration) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		// No usable credential identity: fail open rather than serialize every
		// account onto one gate. The host always supplies an AuthID.
		return func() {}, nil
	}
	ch := g.channelFor(key)
	if timeout <= 0 {
		timeout = upstreamGateAcquireTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-ch })
		}, nil
	case <-timer.C:
		return nil, errUpstreamGateTimeout
	}
}

// acquireUpstreamSlot is the package-level entry point used by the executors.
// A non-empty key is required for throttling; key is derived from the credential.
func acquireUpstreamSlot(authID, authUID string) (func(), error) {
	key := gateKey(authID, authUID)
	release, err := upstreamGateRegistry.acquire(key, upstreamGateAcquireTimeout)
	if err != nil {
		hostLog("warn", "qoder upstream concurrency gate saturated", map[string]any{
			"auth_id":   authID,
			"auth_uid":  authUID,
			"limit":     upstreamMaxConcurrentPerAuth,
			"waited_ms": int(upstreamGateAcquireTimeout / time.Millisecond),
			"error":     err.Error(),
		})
		return nil, err
	}
	return release, nil
}

// hostLog forwards a structured log line to the CPA host log. Best-effort: a
// failure here must never affect request handling, and it is silent when the
// host bridge is unavailable (unit tests).
func hostLog(level, message string, fields map[string]any) {
	if !hostBridgeAvailable() {
		return
	}
	payload := map[string]any{"level": level, "message": message}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}
