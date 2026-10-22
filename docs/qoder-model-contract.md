# Qoder model context and reasoning contract

This change preserves `model_context: {model_id: tokens}`. It introduces no realm-scoped configuration, scheduler, new alias inference, or new supported reasoning values.

## Context evidence and resolution

1. Discovery stores **raw account-specific facts**, keyed by `region + NUL + UID`. Legacy accounts without UID use a SHA-256 digest of the **whole** credential, not its first 16 bytes. Credentials are not exposed by the panel.
2. A valid uniquely marked `context_config.is_default` supplies the account's default. When no context tiers were published, a positive bounded `max_input_tokens` may supply fallback metadata; it is not labelled a catalog default. Tiers without any marked default do not select an arbitrary largest tier. Unknown context stays unknown.
3. `effectiveContextFacts(id, raw)` overlays configured intent only if the **actual account's** tiers include it. Otherwise it keeps that account's default/fallback and reports `context_status: override_rejected` plus `context_requested`.
4. `effectiveModelsForAccount` drives `ModelInfo.ContextLength` on `model.for_auth`. The same resolver drives `contextTierForModel` → `withModelContext` → upstream `model_config.max_input_tokens` on both executor paths. Cached raw facts and on-disk catalogs never bake in an override.
5. Provider-wide `model.static` and the admin catalog retain the candidate union, with the minimum account-effective context for a shared ID. An unknown value among known serving accounts makes the aggregate context unknown (`0`). This is deliberately not a claim that every region or every account has identical defaults.
6. Panel region summaries are conservative across accounts in that region. `account_variants` preserves anonymous per-account context/reasoning facts; `account_count` and `context_status: account_variance` expose disagreements. The panel's `context_options` are global common tiers, not the region union. Incompatible choices remain visible but disabled.

### Freshness and unknown accounts

Refresh TTL is a freshness boundary, not an identity boundary. A failed refresh or expired cache may use **that account's own last-good** catalog and facts. Region status reports `last_good` and `stale_accounts`; account variants report `catalog_state: ready | last_good`.

The additive on-disk `facts_version` marker distinguishes new factual metadata from older inferred fields while retaining schema-v1 compatibility. Legacy catalogs retain IDs and explicit tier/default markers, but their guessed reasoning defaults, unsupported claims lacking field-presence provenance, and no-tier synthesized context values are not promoted to verified facts. No cache files are deleted or rewritten merely by reading them.

An account with no own discovery/last-good catalog returns an empty `model.for_auth` base catalog (not-ready). It never borrows another account's model union, nor treats the static display candidate list as entitlement. Explicit existing overlay additions remain operator additions, with unknown context. Static display candidates are preserved, with unknown context rather than unverified 1M ceilings. No default `models` authorization configuration was added.

If no reliable context fact exists, the body preserves the existing restricted template input window (`180000`) while metadata remains unknown. It does not turn that compatibility fallback into a claimed catalog default or verified capability.

## Global override validation and persistence

`POST /models/context` is **validate-only**:

```json
{
  "success": true,
  "proposal": true,
  "applied": false,
  "persistent": false,
  "id": "shared-model",
  "context_length": 200000,
  "model_context": {"shared-model": 200000},
  "context_options": [200000]
}
```

- Positive proposals must belong to the intersection of every known cached account that serves this model. Missing tier facts veto a global choice. Last-good facts still constrain validation.
- `context_length: null` or omitted proposes clearing this model's override. Clearing is allowed even without a catalog.
- Proposal creation never changes live memory. A rejected/failed host PATCH therefore cannot leave a new transient override behind.
- The panel sends the proposal map through host `PATCH /plugins/qoder/config`. The existing `configure()` → `replaceContextOverrides()` path is the sole production commit path.
- The panel independently reads host `GET /plugins/qoder/config` and plugin `GET /models/context`; it reports success only after both retained the requested map entry/deletion. A runtime `context_states` incompatibility after catalog changes reports “saved, but falling back”, not applied success. Failed or ambiguous readback reports failure and reloads the actual catalog, rather than restoring a made-up optimistic snapshot.
- The panel supports clearing with **恢复各账号默认**, not merely replacing an override with one region's default.
- A config saved before a new incompatible account appeared is not grounds to unload the plugin. Keep syntactically valid intent and expose fallback/incompatibility at runtime.
- Context capacity and output length remain independent. No change is made to `parameters.max_tokens` (template `32768`) or `ModelInfo.MaxCompletionTokens` (existing `8192`).

### Parent-owned integration hooks

- Existing management POST dispatch calls `handleModelContextWrite` unchanged, but now consumes the proposal contract above.
- Management GET returns `model_context: currentContextOverrides()`; `context_states: contextOverrideStates()` exposes each configured model's `requested`, `context_options`, and `status` (`compatible_known_accounts` or `unverified_or_incompatible`). Configured intent is not a persistence receipt by itself.
- Host config schema must describe `model_context` as an object/map (not an array), without introducing a second commit endpoint.
- `configure()` must preserve absent keys, accept explicit `{}` to clear, and avoid plugin-wide unloading for stale/incompatible context intent. Structural parse errors preserve the previous valid snapshot.
- No main executor changes are needed: both existing request paths already call `contextTierForModel` and `withModelContext`.
- Host PATCH has no compare-and-swap revision in the existing contract. In-panel writes are serialized, but concurrent writes from different management clients are still subject to the host's last-writer-wins semantics. This change does not claim a cross-client transaction.

## Reasoning: catalog versus adapter

`thinking.levels` and `thinking.default` describe what the catalog actually says, including `high` and `max`. A switch marked enabled-default is not proof that the highest named effort is the default. An absent `is_reasoning` field with no thinking config is unknown, not unsupported.

`thinking.supported_by_adapter` contains reported levels accepted by the existing adapter (`low`, `medium`, `xhigh`). `thinking.unsupported_by_adapter` contains catalog levels not accepted by it. The panel explicitly labels the catalog default, adapter-forwardable intersection, and unsupported levels.

`normalizeReasoningEffort` remains unchanged: only `low`, `medium`, `xhigh` normalize to a forwarded effort. `high`, `max`, empty and invalid values inject no reasoning override. Catalog defaults are not copied into requests. The existing fixed `model_config.is_reasoning=true` and source behavior remain intact; the adapter does not infer per-model booleans from incomplete metadata, nor start forwarding disabled/off values.

## Pure regression coverage

- Actual `handleModelForAuth` and `handleModelStatic` responses: default, common-tier switch, clear, different CN/Intl defaults, conservative union, incompatible saved override.
- Actual execute and synchronous stream handlers against a loopback fake SSE gateway: both serialize the same per-account effective context; a CN-only 1M tier never leaks to Intl; output `max_tokens` remains independent; `max` effort is not forwarded; fixed `is_reasoning` remains true.
- Discovery JSON parsing: absent reasoning capability, no context default, catalog high/max versus adapter-supported values.
- Unknown accounts, invalid storage, account-local expired last-good, missing-tier veto, same-region differences, and legacy token-prefix collision.
- Existing configuration reload tests and proposal-only creation/deletion tests.
- Panel context click/clear, PATCH failure, host readback mismatch, runtime readback mismatch, no-default rendering, adapter boundaries, and HTML escaping.

Tests use fixtures, temporary storage, and loopback servers. They do not claim live gateway validation of additional reasoning values or deployed behavior.

## 0.9.25 panel presentation

The panel uses concise `high（默认）` / `200K（默认）` labels. A default marker is not selected styling: only the effective context tier is bold/highlighted. Reasoning facts remain read-only plain text. Adapter-forwarding diagnostics and the reset-to-account-default control are not rendered; the backend validation, request adapter limits and explicit configuration-clear API remain intact.
