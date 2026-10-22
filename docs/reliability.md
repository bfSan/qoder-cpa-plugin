# Qoder reliability and model contract (0.9.24)

This release keeps Qoder's CN/Intl DeviceFlow, PKCE, COSY and QoderEncoding protocols. It does not register a scheduler or choose another credential; routing remains with CPA/staged. See [model contract](qoder-model-contract.md) for detailed context and reasoning metadata semantics.

## Confirmed check-in, not optimistic acknowledgement

Manual, scheduled and retry paths share a stable account-lock stripe. Background work checks the current physical credential's disabled state; manual check-in never changes that flag. An explicitly successful Qoder campaign CLAIMED receipt is confirmation, including the idempotent replay case. Replay means the grant already exists, not that another client necessarily made it. A missing activity row by itself is not proof of success. Legacy success responses require a status verification.

Transport uncertainty, 5xx, redirects and malformed POST replies trigger verification of the original campaign/contract rather than blind repeat claims. Only an explicit retryable rejection can lead to a later status-checked claim. CN fallback to the old daily endpoint occurs only after an explicit unsupported campaign GET (404/405), never after an uncertain POST.

Retries are bounded to 1/2/5 minutes, at most three per account per process-local day. Host-list failures also consume budget. Pending checkpoints and consumed retry budgets are protected from capacity eviction; a full 512-entry table rejects new work rather than forgetting uncertain claims. Unconfirmed multi-day campaigns remain verification-only across midnight. Turning check-in off cancels scheduled work; manual disable/deletion cancels that account's retries. These checkpoints, locks and retry budgets are process-local and reset on restart; they do not provide cross-process exactly-once semantics.

There is no check-in history panel. Existing single/batch controls use explicit `confirmed` results and update only matching confirmed account rows.

## Routing and proxy safety

`plugins.configs.qoder.proxy-url` accepts an HTTP/HTTPS/SOCKS5/SOCKS5H proxy. An empty setting inherits the host HTTP bridge. Invalid explicit routing blocks outbound HTTP, but does not intentionally unload the entire plugin.

Once a host HTTP/stream bridge request has been attempted, any bridge failure is returned; it is never reissued directly. When an explicit plugin proxy is selected, login, refresh, billing and model/chat traffic all use that route exclusively. The old OAuth-specific direct client path is removed. Direct compatibility mode is selected only when the bridge is absent, and honors environment proxy configuration.

Direct/proxy requests reject redirect following, so 307/308 cannot replay mutation bodies. Direct/proxy streams stay streaming; cancellation and partial-data terminal errors are retained. Host-managed redirects and RPC deadlines remain the host's responsibility: the current bridge does not carry a per-request deadline field, so plugin context timeouts are not a guaranteed end-to-end wall-clock bound in bridge mode. Proxy/auth request errors must not expose credentials, nonce/verifier or token-bearing URLs.

## Recoverable login tasks

A distinct random state identifies each flow. Pending records preserve Qoder nonce/verifier/region/device key, owner (panel versus host RPC), token response, account result and successful-save checkpoint. Token receipt is checkpointed before subsequent identity lookup/save. A failed save or identity lookup retries the same state without requesting a token already obtained.

On Unix, `Host.AuthDir/.qoder-oauth-pending` uses a private directory, owner-only records, atomic fsync/rename, process locking, HMAC record authentication and bounded files/size/TTL. Credentials in pending records are not encrypted; file ownership and access permissions matter. HMAC does not protect against an attacker with access to the same owner's key. Missing Host directory or non-Unix platforms explicitly use memory-only mode, rather than writing into arbitrary HOME/CWD locations. Unsafe supplied storage fails closed for login while model service stays available.

The panel stores only state/deadline/region in sessionStorage, not tokens, PKCE secrets or authorization URLs. It can resume same-tab sessions after a refresh. A task is not independently polled while the page is closed. Host RPC and panel save ownership are separate: host RPC returns AuthData for the host to save; the panel reports success only after its own save succeeds.

Upstream token receipt and local fsync are not one transaction. A crash between them is inherently uncertain. Similarly, a crash after host save but before the saved marker can cause another idempotent write of the same canonical credential file after restart. No strict distributed exactly-once guarantee is claimed.

## Context and model identity

Context selection now projects into both account-specific model registration and `model_config.max_input_tokens`. Default selection follows that account's explicit catalog evidence. Global overrides use the intersection of known serving accounts' supported tiers and are revalidated against the actual account at execution. An incompatible override falls back to that account's own evidenced value and exposes its state rather than pretending it applied.

POST `/models/context` is a proposal only (`persistent:false`, `applied:false`); it does not mutate runtime. The panel PATCHes host configuration, reads the persisted configuration and then the plugin runtime state before reporting success. Cross-client PATCHes remain host last-writer-wins, not an atomic CAS transaction.

An account with neither a usable current directory nor its own last-good snapshot no longer borrows another account's entitlements. The global display can retain candidate IDs, but unknown context is zero/unknown metadata rather than an invented capability. Output budget is separate and unchanged. Unknown request facts still leave the legacy 180000-token template field untouched; this is not a promise about upstream accepted limits.

Directory reasoning defaults and adapter-supported request values are shown separately. The adapter continues to forward only the verified low/medium/xhigh values; high/max catalog metadata is not silently mapped or enabled. No catalog default is injected into outgoing requests.

## Quota and cache recovery

Existing strict daily-count classification, queue admission retry behavior, execution-time guards and manual clear are retained. Additional observations expose reason, model/account scope, occurrence time, next probe time and any explicit reset evidence. Only explicit shared-credit exhaustion can create an account-wide hold. Zero balance, missing packages and one model's quota do not disable the credential.

Reset time comes from valid explicit retry/reset evidence, otherwise finite TTL is a probe interval, not a guessed midnight reset. A fresh positive credit observation can clear only shared-credit exhaustion, not daily/model quota; a read started before the hold cannot clear newer state. Manual clear does not alter manual disabled intent. Unknown/manual/session-dead disabled credentials are not automatically reenabled; legacy automatic exhaustion migration requires explicit reason evidence.

Cache updates use observation timestamps and CAS merging. An old dashboard flight cannot overwrite a newer confirmation/credit read, successful check-in grace is preserved, and fallback balances do not get fabricated fresh timestamps. Repeated observation of an existing receipt does not indefinitely extend grace.

Quota state is process-local. A restart clears holds; expiry permits a later request to probe and does not prove that upstream capacity has recovered.

## Validation and deployment

Tests use local fake hosts/HTTP servers and synthetic panel responses. No real check-in, token exchange or paid long-context load is required. Run:

```sh
go test -race -count=1 ./...
go vet ./...
node --test panel.test.js panel.reliability.test.js
```

When deploying, back up the loaded shared object, install to a new inode using atomic rename, restart CPA, and verify the registered version, loaded inode, checksum, service health, and panel contract. Never overwrite an already loaded `.so` in place. Runtime discovery/metadata checks are distinct from proving actual upstream model request acceptance.
