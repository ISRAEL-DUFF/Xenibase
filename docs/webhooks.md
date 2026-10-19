# Webhooks and scheduled jobs

## Webhooks

A webhook POSTs the changes of chosen tables to a URL: Database →
Webhooks, `xenibase webhooks create`, or `POST /api/v1/projects/{id}/webhooks`.

| Field | Notes |
| --- | --- |
| Name | Letters, digits, spaces, dots, dashes, underscores. |
| Tables | One or more of the project's tables (`orders`, `billing.invoices`). |
| Events | Any of `INSERT`, `UPDATE`, `DELETE`. |
| Columns | Optional: an `UPDATE` fires only when one of these columns changed. |
| URL | `https://` to a public address. Plain `http://` and internal addresses only to hosts the platform admin allow-listed for your organisation. |
| Headers | Optional static headers (an auth header for your receiver); stored encrypted and never shown again. |
| Signing secret | Generated, shown once, rotatable. |
| Description | Optional free text, one line, up to 500 characters. |
| Metadata | Optional string tags (up to 16; keys of 1 to 40 letters, digits, `_ . : -`; values up to 500 characters), for a tool to mark the webhooks it made. |

### How it works

Saving the first webhook installs a `xenibase` schema in the project's
database, owned by Xenibase (your role can't read or change it), with an
outbox table and a trigger function. Each configured table gets triggers
that write every change to the outbox **in the same transaction**: a
rolled-back change never produces an event, and a committed one always
does. Xenibase's delivery worker wakes on each commit (and checks every five
seconds), and posts the events of each webhook in commit order.

The `xenibase` schema name is reserved: a schema of that name you created is
replaced. Your role owns its tables, so it can drop Xenibase's triggers;
Xenibase notices and marks the webhook **broken** rather than putting them
back silently. Saving the webhook again reinstalls them.

### Payload

```json
{
  "id": "evt_3f2a9c1b7d4e_42",
  "webhook": "orders-to-slack",
  "project": "p_k2f9a7bq3d",
  "project_id": "bf497edb-3bf5-4189-a124-52731c45d74a",
  "table": "public.orders",
  "type": "UPDATE",
  "record": { "id": 7, "status": "paid" },
  "old_record": { "id": 7, "status": "new" },
  "committed_at": "2026-10-01T12:00:00Z",
  "primary_key": { "id": 7 }
}
```

| Field | |
| --- | --- |
| `id` | The event's id, also in `Xenibase-Event-Id`; stable across retries and replays. |
| `webhook` | The webhook's name. |
| `project` | The project's **database name** (`p_…`), as in its connection strings. |
| `project_id` | The project's id in the API (`/api/v1/projects/{id}`). |
| `table` | `schema.table`. |
| `type` | `INSERT`, `UPDATE`, `DELETE`, or `TEST`. |
| `record` | The row after the change, every column; `null` for a `DELETE`. |
| `old_record` | The row before the change, **every column** (not only the changed ones); `null` for an `INSERT`. |
| `committed_at` | When the transaction committed. |
| `primary_key` | The row's primary key columns and values, on every change event of a table that has one. Absent for a table without a primary key, and for `TEST`. |
| `truncated` | Present and `true` when the rows serialise to more than 256 KB: `record` and `old_record` are then `null`; fetch the row by `primary_key`. A table without a primary key can't be refetched this way. |

Values are the row's `to_jsonb`: `numeric` is a JSON number with all its
digits (parse it as a decimal), timestamps are ISO 8601 strings, arrays
and `jsonb` are JSON. **Send test event** posts `"type": "TEST"` with
`"record": {"message": "A test event from Xenibase"}` and an empty `table`.
New fields may be added; ignore the ones you don't know.

Signed samples of each kind, with the throwaway secret that signed them,
are in [integrations/fixtures/webhooks](integrations/fixtures/webhooks):
check your verifier and parser against them (use the timestamp in each
signature as "now").

Each request carries `Xenibase-Event-Id`, `Xenibase-Webhook`, your static
headers, and `Xenibase-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "t.body">`.
During a secret rotation with an overlap the header has one `v1=` per
secret: `t=<unix>,v1=<new>,v1=<old>`.

### Verifying the signature

Check the HMAC of `"<t>.<raw body>"` with the webhook's secret against
**each** `v1` value (accept if any matches), compare in constant time, and
refuse timestamps more than five minutes away (replays). De-duplicate on
`Xenibase-Event-Id`: delivery is at least once.

Go:

```go
func verify(secret, header string, body []byte, now time.Time) bool {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || math.Abs(now.Sub(time.Unix(n, 0)).Minutes()) > 5 {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	want := []byte(hex.EncodeToString(m.Sum(nil)))
	for _, sig := range sigs {
		if hmac.Equal(want, []byte(sig)) {
			return true
		}
	}
	return false
}
```

Node:

```js
import { createHmac, timingSafeEqual } from "node:crypto";

function verify(secret, header, rawBody, now = Date.now()) {
  const parts = header.split(",").map((kv) => kv.split("="));
  const t = parts.find(([k]) => k === "t")?.[1];
  const sigs = parts.filter(([k]) => k === "v1").map(([, v]) => v);
  if (!t || sigs.length === 0 || Math.abs(now / 1000 - Number(t)) > 300) return false;
  const want = createHmac("sha256", secret).update(`${t}.${rawBody}`).digest("hex");
  return sigs.some((s) => s.length === want.length && timingSafeEqual(Buffer.from(want), Buffer.from(s)));
}
```

Verify the raw body, before parsing it.

### Rotating the secret

`POST /api/v1/projects/{id}/webhooks/{webhook_id}/rotate-secret` (or
**Rotate secret** on the webhook's page) returns the new secret once:
`{"secret":"whsec_…"}`. With no body the old secret stops at once, so
deliveries in flight fail verification at a receiver that only has the
old one. To rotate without that gap, send an overlap:

```json
{"overlap_seconds": 3600}
```

For up to that long (at most 86,400, a day) every delivery is signed with
both secrets (two `v1=` values), and the answer and the webhook carry
`previous_secret_expires_at`. Store the new secret at your receiver, keep
accepting the old one until then, and drop it after. Rotating again ends
any earlier overlap.

### Delivery

- **Ordered per webhook**, in commit order. A failing event holds back the
  ones behind it for that webhook only.
- **Retries** on network errors, timeouts (10 s), `429` and `5xx`, with
  exponential backoff (10 s doubling to 2 h, with jitter) for 24 hours;
  then the event becomes a **dead letter**. Other `4xx` answers, and
  redirects (never followed), dead-letter at once.
- **Dead letters** stay 30 days: replay them one by one or all at once
  (`xenibase webhooks replay`); they are sent after the events waiting.
- After **50 failures in a row** the webhook is paused and the project's
  admins and the organisation's owners are emailed. A paused webhook
  records no new changes (its triggers are disabled); resuming sends what
  was queued.
- More than **100,000** queued events email the project's admins.
- Your plan limits **deliveries per minute for the whole organisation**
  (Personal 60, Team 300), shared by every webhook in it, including ones
  another tool created through the API. Beyond it events wait in the
  queue; none are dropped. A backlog of N events drains in about N ÷
  the limit minutes.
- The **delivery log** (status, latency, the first 4 KB of the answer) is
  kept 7 days.

While a project is promoted, demoted, restored or reset, delivery pauses
and resumes afterwards; the outbox moves with the data. Branches and
projects restored from a backup don't get the webhooks. A restore in place
reinstalls the triggers with an empty queue: events from the restored past
aren't sent. Deleting the project deletes its webhooks.

### Managing webhooks from another tool

A tool that sets up webhooks for its users (a workflow or integration
platform) uses the same API with an **API token restricted to the
projects it needs**, scope `write` (all webhook calls need it, reads
included). Tokens skip the CSRF header the web UI sends; see
[cli.md](cli.md) for creating one and the OpenAPI file
(`api/openapi.yaml`, `bearerAuth`) for the scheme:
`Authorization: Bearer xb_…`. Token calls count against the token's and
the organisation's request limits (`429 rate_limited` with
`Retry-After`), and the deliveries against the organisation's delivery
rate above.

| Call | | Errors |
| --- | --- | --- |
| `POST /projects/{id}/webhooks` | Create. Answers `201` with the webhook and its `secret`, the only time it is shown. | `400 bad_request` (a field), `409 conflict` (the name is taken, or a table is busy: retry), `409 quota_exceeded`, `409 outbound_disabled` |
| `GET /projects/{id}/webhooks`, `GET …/{webhook_id}` | List, get. Never return the secret or header values (`header_names` only). | `404 not_found` |
| `PATCH …/{webhook_id}` | Change the fields sent; the rest stay. **Lists are replaced, not merged**: `tables`, `events`, `columns`, `headers` (`{}` removes them) and `metadata` (`{}` removes it). | as create |
| `DELETE …/{webhook_id}` | Delete, with its queue and dead letters. No `confirm` needed. `204`. | `404 not_found` |
| `POST …/{webhook_id}/rotate-secret` | See above. | `400 bad_request` (overlap out of range) |
| `POST …/{webhook_id}/test` | Send a `TEST` event now; answers with the receiver's status. | |
| `GET …/{webhook_id}/deliveries`, `POST …/replay` | The delivery log; replay dead letters. | |

- **Names are unique within a project.** Mark the webhooks your tool owns
  with `metadata` (for example `{"created_by":"taskiem","taskiem:workflow":"wf_1"}`)
  and find them by listing, so one a person renamed is still yours.
- **State** is `status`: `healthy`, `failing` (recent deliveries failed,
  still retrying), `paused` (50 failures in a row, or `enabled: false`), or
  `broken` (someone dropped Xenibase's triggers; `status_reason` says
  which). `PATCH {"enabled": true}` resumes a paused webhook and sends
  what was queued; saving it (any `PATCH`) repairs a broken one.
- Calls that change a webhook reach the project's database. While a Free
  project is paused or archived for inactivity they answer `503
  project_resuming` (`Retry-After: 10`) or `503 project_restoring`
  (`Retry-After: 60`) and wake it; retry after the delay. Reads still
  answer.
- Every error is `{"code":"…","message":"…"}` (the management API's flat
  shape; the data API wraps it in `error`); the full list, with which
  codes are worth retrying, is in [errors.md](errors.md).

## Scheduled jobs

A job runs SQL, or calls a URL, on a schedule: Database → Scheduled jobs,
`xenibase jobs create`, or `POST /api/v1/projects/{id}/jobs`.

| Field | Notes |
| --- | --- |
| Schedule | A 5-field cron expression (`minute hour day month weekday`, with ranges, lists, steps, `jan`–`dec`, `sun`–`sat`, and `@hourly`, `@daily`, `@weekly`, `@monthly`), in a time zone (default UTC). |
| SQL | Runs as the project owner, in one transaction, with the timeout as `statement_timeout`. A storage soft lock (read-only) applies to it. |
| HTTP | Method, URL, headers, body; the same address rules as webhooks, signed with the job's secret (shown once), with a `Xenibase-Job` header. |
| Timeout | Default 5 min for SQL, 30 s for HTTP; at most 1 hour. |
| Overlap | Skip a run while the previous one is going (default), or queue one. |

- Your plan limits the number of jobs (Personal 20, Team 50), how often
  one may run (every 5 minutes on Personal, every minute on Team), and HTTP
  job runs per hour (120, 600; runs beyond it are skipped).
- **Missed runs are not caught up**: after Xenibase was down, the next
  scheduled time runs and the history shows the gap.
- Runs are skipped (and recorded as such) while the project is moving
  between tiers, restoring or resetting, while the organisation is
  suspended, and, for HTTP jobs, while its outbound traffic is off.
- **History**: start, duration, status, rows affected or HTTP status, and
  the error; kept 30 days, at most 1,000 runs per job. **Run now** starts
  one at once. Three failures in a row email the project's admins.
