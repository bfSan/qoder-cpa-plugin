// host_auth.go wraps the host's auth-store RPC (host.auth.list / get /
// get_bundle). These are the only paths the plugin uses to read auth files;
// writes go through hostAuthPersist / hostAuthPersistMigrate in lifecycle.go.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all qoder credentials known to the host. The
// "qoder-" prefix matches canonical qoder-<uid>.json plus the kept legacy
// names qoder-cn-<uid>.json / qoder-intl-<uid>.json (merged plugin, v0.10.0)
// and the legacy single files qoder-cn.json / qoder-intl.json.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	// Fresh slice — resp.Files[:0] would alias the RPC response's backing
	// array (P1-3: fragile pattern, safe today but could break if resp is
	// ever cached/reused).
	//
	// Filter by filename prefix, NOT by Type/Provider: many existing auth
	// files on disk don't carry a "type"/"provider" field (they were written
	// before that convention), and EqualFold("", providerName) returns false
	// for them — meaning we'd incorrectly include files that have the
	// workbuddy- prefix but no type field. Filename prefix is the only
	// reliable cross-version discriminator.
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	prefix := providerName + "-"
	for _, f := range resp.Files {
		if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			out = append(out, f)
		}
	}
	return out, nil
}

// hostAuthGet fetches the credential JSON for one auth index.
func hostAuthGet(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// hostAuthGetBundle is one host.auth.get for both storage and physical metadata
// (avoids the previous double-RPC in dashboard: get + getPhysical).
func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}

// runtimeAuthLabel reads the label CPA currently exposes for one runtime auth
// record. host.auth.save rebuilds that record from top-level "type"/"email"
// only, so a freshly saved nickname is not visible until the file watcher
// re-parses the file through auth.parse. Callers that change a nickname wait
// on this value so the native auth page cannot keep showing the provider name.
func runtimeAuthLabel(authIndex string) (string, error) {
	return runtimeAuthLabelFn(authIndex)
}

var runtimeAuthLabelFn = runtimeAuthLabelDefault

func runtimeAuthLabelDefault(authIndex string) (string, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGetRuntime, body)
	if err != nil {
		return "", err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return "", fmt.Errorf("host.auth.get_runtime: bad envelope")
	}
	var resp pluginapi.HostAuthGetRuntimeResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Auth.Label), nil
}

// waitForRuntimeAuthLabel blocks briefly until CPA exposes want as the runtime
// label. It tolerates a missing runtime read so a host without the callback
// still persists the rename; the caller only uses the error to report a
// delayed sync, not to roll the file back.
func waitForRuntimeAuthLabel(authIndex, want string, timeout time.Duration) error {
	want = strings.TrimSpace(want)
	if authIndex == "" || want == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		label, err := runtimeAuthLabel(authIndex)
		if err == nil {
			last = strings.TrimSpace(label)
			if last == want {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if last == "" {
				return fmt.Errorf("runtime label did not sync to %q", want)
			}
			return fmt.Errorf("runtime label is %q, want %q", last, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
