# Error codes

Every error from Xenibase's APIs is JSON with a stable `code` and a message
for people. The management API (`/api/v1`) answers a flat body:

```json
{"code":"quota_exceeded","message":"the organisation has 10 projects, its plan's limit","quota":{"limit":"projects","used":10,"max":10}}
```

The data API (`https://<ref>.<domain>/data/v1`, `/auth/v1`,
`/storage/v1`) wraps the same fields in `error`, and adds a `request_id`
and `details` where they help (the Postgres `pg_code`, the batch
`operation`, the `table`):

```json
{"error":{"code":"unique_violation","message":"orders_email_key","details":{"pg_code":"23505"},"request_id":"req_…"}}
```

Codes are part of
the contract (see [Deprecation](#deprecation)); messages may change.

**Retry** says whether sending the same request again can succeed without
anyone changing anything:

- **yes**: wait for `Retry-After` (seconds) when it is sent, otherwise back
  off (1 s doubling, with jitter), then retry. Every `429` and every
  retryable `503` carries `Retry-After`.
- **once**: retry once after a short wait; if it fails again, stop.
- **no**: fix the request, the permissions or the data first.

A timeout or a lost connection with no answer at all is not an error code:
for a write you don't know whether it happened. On the data API, send an
[`Idempotency-Key`](backend-services.md#retrying-writes-safely-idempotency-key)
so the retry is safe.

The class column groups the codes the way an integration usually handles
them: **validation**, **auth** (the caller isn't who it says, or may not do
this), **not found**, **conflict**, **limit** (a plan or quota; changes when
the plan does), **rate** (slow down), **unavailable** (temporary).

## Management API (`/api/v1`)

These apply to every endpoint, including webhooks (`/projects/{id}/webhooks`),
the SQL endpoint (`/projects/{id}/sql`) and table rows
(`/projects/{id}/tables/…`).

| Code | Status | Class | Retry | Meaning |
| --- | --- | --- | --- | --- |
| `bad_request` | 400 | validation | no | A field is missing or invalid; the message names it. |
| `invalid` | 400 | validation | no | As `bad_request`, for tokens. |
| `confirm_required` | 400 | validation | no | A destructive action by token needs `"confirm": "<name>"`. Webhook deletion doesn't. |
| `not_editable`, `bad_filter` | 400 | validation | no | Table rows: the column can't be written, or the filter is malformed. |
| `unauthenticated` | 401 | auth | no | No session or token. |
| `invalid_token` | 401 | auth | no | The API token is unknown, revoked or expired. |
| `insufficient_scope` | 403 | auth | no | The token lacks the scope (`read`, `write`, `admin`) or is restricted to other projects. Webhook calls all need `write`. |
| `token_not_allowed` | 403 | auth | no | Only the web UI can do this. |
| `forbidden`, `permission_denied` | 403 | auth | no | The user's role in the organisation or project doesn't allow it. |
| `csrf` | 403 | auth | no | A browser session's change without `X-CSRF-Token`. Tokens never need it. |
| `org_suspended` | 403 | auth | no | The organisation is suspended. |
| `plan_required` | 403 | limit | no | The plan doesn't include this. |
| `console_disabled`, `console_read_only` | 403 | auth | no | The project's SQL console is off, or read-only (send `read_only: true`). |
| `not_found` | 404 | not found | no | No such project, webhook, …, or not visible to the caller. |
| `conflict` | 409 | conflict | no* | The state doesn't allow it: a webhook name is taken, the project is busy with another operation, a table is locked by a long transaction. *The last two clear by themselves: retry after a few seconds at most a few times. |
| `quota_exceeded` | 409 | limit | no | An organisation quota (webhooks, jobs, projects, …); `quota` says which. |
| `outbound_disabled` | 409 | conflict | no | The organisation's outbound traffic is off (an admin's decision). |
| `capacity_pending_approval` | 409 | unavailable | later | Capacity is being added; the message says when to try again. |
| `rate_limited` | 429 | rate | yes | Too many requests for the token (default 600 a minute) or the organisation's tokens (1,200). `Retry-After` is sent. |
| `internal` | 500 | unavailable | once | A fault on Xenibase's side; report it if it repeats. |
| `project_resuming` | 503 | unavailable | yes | A Free project was paused for inactivity; this request woke it. `Retry-After: 10`. |
| `project_restoring` | 503 | unavailable | yes | A Free project was archived; this request started restoring it (a minute or two). `Retry-After: 60`. |
| `no_capacity` | 503 | unavailable | later | No room for a new project now. |
| `unavailable` | 503 | unavailable | no | The feature isn't set up on this server (its operator hasn't configured it). Not temporary: no `Retry-After`. |

**Table row changes** (`POST /projects/{id}/tables/…/changes`) report a
row that changed since it was read (by `xmin`) in the answer's `conflict`
field, with nothing applied: read the row again.

**The OAuth endpoints** for third-party apps (`POST /oauth/token`,
`POST /oauth/revoke`, see [OAuth for third-party apps](oauth-apps.md))
answer in OAuth's own shape, `{"error": "…", "error_description": "…"}`
(RFC 6749 section 5.2), with these codes:

| `error` | Status | Class | Retry | Meaning |
| --- | --- | --- | --- | --- |
| `invalid_request` | 400 | validation | no | A parameter is missing or malformed. |
| `invalid_client` | 401 | auth | no | Unknown or disabled client, wrong secret, or a secret from a public client. |
| `invalid_grant` | 400 | auth | no | The code is unknown, expired, already used (which revokes its token), another client's, or the redirect URI or PKCE verifier doesn't match. |
| `unsupported_grant_type` | 400 | validation | no | Only `authorization_code`. |
| `slow_down` | 429 | rate | yes | Too many attempts from one address; `Retry-After` is sent. |
| `server_error` | 500 | unavailable | once | A fault on Xenibase's side. |

The consent page's own calls (`/oauth/authorize`) and the client registry
use the management codes above (`invalid` for a bad request, `not_found`,
`reauth_required`). A token issued this way fails like any other:
`401 invalid_token` once revoked or expired.

**The SQL endpoint** answers `200` for a query that ran or that Postgres
refused: check `error` in the result (`{"error":{"code":"42P01","message":…,
"position":…}}`, the SQLSTATE in `code`). The statuses above are for the
request itself. For writes from another tool, prefer the data API below:
bound values, one operation per call, non-2xx errors, and idempotency
keys.

## Data API (`https://<ref>.<domain>`)

| Code | Status | Class | Retry | Meaning |
| --- | --- | --- | --- | --- |
| `invalid_filter`, `invalid_select`, `invalid_order`, `invalid_limit`, `invalid_offset`, `invalid_cursor`, `invalid_count`, `invalid_return`, `invalid_resolution`, `invalid_max_affected`, `unknown_parameter`, `too_many_filters`, `embed_too_deep`, `invalid_body`, `invalid_path` | 400 | validation | no | The request is malformed; the message says how. |
| `invalid_value`, `type_mismatch`, `unsupported_operator` | 400 | validation | no | A value doesn't fit its column (Postgres refused it). |
| `unknown_column`, `unknown_relation`, `ambiguous_relation`, `no_single_key`, `no_matching_function`, `ambiguous_function`, `not_writable` | 400 | validation | no | The schema doesn't have what was named. |
| `filter_required` | 400 | validation | no | An update or delete needs a filter or a key. |
| `too_many_rows` | 400 | validation | no | More rows would change than `max_affected`; nothing changed. |
| `query_too_expensive` | 400 | validation | no | The plan's cost is over the project's limit; add an index or a filter. |
| `invalid_idempotency_key` | 400 | validation | no | The key is empty, over 255 characters, or has spaces. |
| `idempotency_needs_user` | 400 | validation | no | Keys need the secret key or a signed-in user. |
| `database_error` | 400 | validation | no | Another error from Postgres; `details.pg_code` has the SQLSTATE. |
| `key_required`, `invalid_key` | 401 | auth | no | No API key, or an unknown one. |
| `invalid_token` | 401 | auth | no | The user's access token is invalid or expired: refresh it. |
| `rls_required` | 403 | auth | no | With the publishable key, a table without row-level security can't be written. |
| `permission_denied` | 403 | auth | no | Row-level security or a grant refused it. |
| `secret_key_in_browser`, `origin_not_allowed` | 403 | auth | no | The secret key from a browser, or an origin not in the project's CORS list. |
| `project_suspended` | 403 | auth | no | The organisation is suspended. |
| `read_only` | 403 | conflict | no | The database is read-only (its storage limit). |
| `unknown_table`, `unknown_function`, `not_found`, `project_not_found`, `unknown_host` | 404 | not found | no | Nothing there, or the row isn't visible to the caller. |
| `method_not_allowed`, `volatile_function` | 405 | validation | no | Wrong method; a function that writes needs `POST`. |
| `unique_violation`, `foreign_key_violation` | 409 | conflict | no | A constraint; an upsert (`on_conflict`) avoids the first. |
| `body_too_large`, `result_too_large` | 413 | validation | no | Over 8 MB in, or 10 MB out. |
| `not_null_violation`, `check_violation` | 422 | validation | no | A constraint. |
| `idempotency_key_reused` | 422 | conflict | no | The key was used in the last 24 hours for a different request. |
| `rate_limited`, `spend_cap_rate_limited` | 429 | rate | yes | The project's per-key or per-address rate, or the reduced rate after the spend cap. `Retry-After: 1`. |
| `plan_limit_reached` | 429 | limit | no | The month's data API requests are used up; `Retry-After` is the seconds until the month ends. |
| `project_resuming` | 503 | unavailable | yes | Paused or archived for inactivity, and this request woke it. `Retry-After: 10`; an archived project takes a minute or two to restore, so keep retrying. |
| `project_paused` | 503 | unavailable | yes | Paused for inactivity, and this request carried none of the project's API keys, so it didn't wake it. Send the key (or connect to the database) to resume it. `Retry-After: 10`. |
| `database_unavailable` | 503 | unavailable | yes | The database can't be reached for the moment. `Retry-After: 5`. |
| `retry`, `starting` | 503 | unavailable | yes | The edge is reloading the project's configuration, or starting. `Retry-After: 1` or `2`. |
| `idempotency_unavailable` | 503 | unavailable | yes | The project's schemas are being upgraded; keys work within minutes. `Retry-After: 60`. |
| `statement_timeout` | 504 | unavailable | once | The query ran past the project's statement timeout. Retrying the same query usually times out again. |

Auth (`/auth/v1`) and storage (`/storage/v1`) use the codes of the clients
they are compatible with; see [backend-services.md](backend-services.md#auth)
and [backend-services.md](backend-services.md#storage).

## Deprecation

The public contracts are the files in this repository:
`api/openapi.yaml` (management API), the data API as documented in
[backend-services.md](backend-services.md) and served per project at
`GET /data/v1/openapi.json`, [webhooks.md](webhooks.md) (payloads,
headers, signatures), [cli.md](cli.md), and the codes on this page.
Within a major version, fields and codes are only added; a removal or a
change of meaning is announced in [CHANGELOG.md](../CHANGELOG.md) and kept
working for at least 6 months. Clients should ignore fields and treat
codes they don't know by their status (`4xx` no retry, `429` and `503`
retry).
