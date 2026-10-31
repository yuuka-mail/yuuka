# Worker ↔ Go contract vectors

Shared test vectors for the contract in [docs/design/architecture.md §2.2](../../docs/design/architecture.md).
Both the Go tests and the Worker tests load these files; neither side may keep its own copy
of the expected values. A format change is a one-way door (see AGENTS.md): bump the version
directory (`v2/`) instead of editing `v1/` in place, and keep `v1/` passing for as long as
either side still accepts it.

The JSON files are the source of truth and are edited by hand. All strings are UTF-8;
`json` / `body` fields hold the exact bytes that go over the wire or into R2.

| File | What it pins down | How a test uses it |
|---|---|---|
| `v1/markers.json` | The delivery marker (`pending/<id>`, webhook body) | Parsers: every `valid[].json` must parse to `valid[].marker`, and every `invalid[].json` must be rejected (`reason` is for humans). Writers: serializing `valid[].marker` must equal `json` byte for byte (field order as listed, no whitespace) — except cases whose `json` carries fields the marker doesn't have (e.g. `unknown-field-ignored`), which exist only to test that parsers ignore unknown fields |
| `v1/keys.json` | R2 key layout | `received_at` + `id` must produce `raw_key` and `pending_key` (UTC year/month) |
| `v1/metadata.json` | The `yuuka-marker` custom metadata on raw objects | base64url without padding of `marker_json` must equal `yuuka_marker`, and decode back |
| `v1/hmac.json` | Webhook signature | `sign[]`: signing `body` with `secret` at `timestamp` gives `signature`. `verify[]`: with the given `secrets` and clock at `now`, verifying `signature` over `body` must return `valid`. The window is `window_seconds` |

Secrets and addresses here are test fixtures, not real values.
