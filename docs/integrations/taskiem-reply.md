From Xenibase to the Taskiem team · reply to the update of 9 October 2026

# Xenibase's answers to Taskiem

Thank you for the clear list. Everything below is in the repository,
which is the published contract you build from. Where we changed Xenibase
for you, the change is merged with tests, and the docs say how it works.
Your IDs are kept.

We will send no tokens, keys or secrets with this reply, or with any
other message. When you are ready for each one, our account holder will
enter it on Taskiem's Connections page. If that isn't possible, we will
use a one-time password-manager share, and you confirm it by fingerprint.

## At a glance

| ID | Ask | Answer |
| --- | --- | --- |
| S1 | Sandbox, token, base URL | **People:** we set it up and enter the credentials on your Connections page (below). |
| S2 | Signed sample deliveries | **Done:** `docs/integrations/fixtures/webhooks`, checked in CI. |
| S3 | A contact | **People:** named below once confirmed; they will review your `docs/integrations/xenibase.md`. |
| P1-G1 | Safe row writes | **Exists:** the data API's row writes. There will be no parameterised SQL endpoint. |
| P1-G3 | Frozen webhook contract | **Done:** `docs/webhooks.md#managing-webhooks-from-another-tool`, plus `description` and `metadata`. |
| P1-G4 | Error list, `Retry-After` | **Done:** `docs/errors.md`, which marks the retryable codes. Every `429` and retryable `503` sends `Retry-After`. |
| P1-G2 | `Idempotency-Key` | **Done:** on every data API write, with a 24-hour window. |
| C1 | Publish V3/V4 contracts | **Partly:** the data API, auth, storage and paused projects are published. There are no billing events. |
| Q | 13 questions | Answered in section 3. |
| P2-G1 | OAuth consent | **Done (17 October 2026):** `docs/oauth-apps.md`. A platform admin registers Taskiem as an OAuth client; consent issues an ordinary project-restricted token. |
| P3-G1/G2 | Data API; projects by token | **Both exist today.** |
| P4 | Contract tests, operations events, dedicated instances | Section 4. |

## 0. Testing now

**S1. Sandbox.** We will make an organisation, `taskiem-sandbox`, with two
projects:

- `orders`, keyed by `id bigserial` with text, numeric, boolean, array,
  jsonb and timestamptz columns;
- a second project with a table that has no primary key, so you can test
  that case.

Backend services will be on in both. You'll get two credentials, both by
the route above:

- an API token restricted to those two projects, with scopes `read` and
  `write`;
- each project's secret key (`xb_sec_…`), which the data API's writes
  need (see P1-G1).

Q20 has the base URL.

**S2. Samples.** `docs/integrations/fixtures/webhooks/` has five real
deliveries, each with the headers, the exact signed body and the
throwaway secret that signed it:

- `insert.json`
- `update.json` (with `old_record`)
- `delete.json`
- `insert-truncated.json` (over 256 KB)
- `test.json`

Verify each body as it is, using the timestamp in its signature as "now".
Our CI re-captures a delivery of each kind and fails if the published
samples no longer match (`TestWebhookFixtures`). So if the shape changes,
the samples change with it.

**S3. Contact.** We'll name one person for contract questions, who will
also review your connector guide.

## 1. For the first release

### P1-G1: a safe way to write rows

Use the **V4 data API** (`https://<ref>.<api-domain>/data/v1`). It already
has the row-write endpoints you describe, documented in
`docs/backend-services.md#writing-data`:

- **insert:** `POST /data/v1/{table}`, one row or up to 1,000.
- **upsert:** the same `POST` with `?on_conflict=col,…` (and
  `resolution=ignore` to skip rows that conflict).
- **update and delete:** `PATCH` and `DELETE` with the same filter
  grammar as reads (`where=status:eq:new`), or `/data/v1/{table}/{key}`.
  **A filter or key is required** (`400 filter_required`).
- **guard:** `max_affected` (default 1,000). If more rows would change,
  the call answers `400 too_many_rows` and changes nothing.
- **returning:** `select=…` and `return=representation|minimal`.
- **functions:** `POST /data/v1/rpc/{name}`.
- **batches:** `POST /data/v1/batch` runs up to 50 operations in one
  transaction.
- **values are bound**, never put into SQL text.
- **errors are non-2xx**, carry the SQLSTATE in `details.pg_code`, and
  map constraint failures to codes: `unique_violation` 409,
  `not_null_violation` 422, and so on.

Each project also serves its own OpenAPI document at `GET
/data/v1/openapi.json`, generated from its schema.

It authenticates with the project's **secret key** in an `apikey` header.
That key bypasses row-level security, as a server-side integration
expects. The management API token can't call the data API. An `admin`
token can create keys, but we'd rather you didn't ask for one. Backend
services have to be on for the project, which is one call by its owner or
one click.

The SQL endpoint stays as it is, with no parameters, several statements
allowed, and SQL errors inside a `200`. Use it only for read-only queries
the customer writes.

### P1-G3: the webhook contract

It is pinned down in `docs/webhooks.md`, under *Payload*, *Rotating the
secret* and *Managing webhooks from another tool*. In short:

- **Fields:** as in `api/openapi.yaml`. Webhooks now have a
  `description` (one line) and `metadata`, string tags you can set and
  search by: up to 16 keys of `[A-Za-z0-9_.:-]{1,40}`, values up to 500
  characters. For example, `{"created_by":"taskiem","taskiem:workflow":"wf_1"}`.
- **PATCH** changes only the fields sent. Lists (`tables`, `events`,
  `columns`, `headers`, `metadata`) are **replaced**, not merged.
- **The secret** is returned once, in the `201` from create. Later,
  `rotate-secret` returns a new one once.
- **State** is in `status` on every get and list: `healthy`, `failing`,
  `paused` (after 50 failures in a row, or `enabled: false`), or
  `broken` (someone dropped our triggers; `status_reason` says which).
  `PATCH {"enabled": true}` resumes a paused webhook and sends what was
  queued. Any `PATCH` repairs a broken one.
- **Errors per call** are in the same section, and every code is in
  `docs/errors.md`.

## 2. Wanted soon

### P1-G2: `Idempotency-Key`

This is done for all data API writes: table `POST`, `PATCH` and `DELETE`,
batches, and `POST` to functions.

- **Repeats:** a repeat of the key within 24 hours returns the first
  status and body with `Idempotent-Replayed: true`, and writes nothing.
- **Different request, same key:** a different method, path, query or
  body with the same key gets `422 idempotency_key_reused`.
- **Recorded on commit:** the key is stored in the write's own
  transaction, so it exists exactly when the write committed. If the
  write failed or the connection dropped before the commit, no key is
  left behind, and the retry runs the write.
- **Concurrent repeats** wait for the first request, then get its
  answer.
- **Scope:** keys belong to the caller (the secret key, or a signed-in
  user).

So with a key, a plain insert is safe to retry. It isn't available on the
management API. You don't need it there: webhook names are unique, so a
repeated create gets `409 conflict`, and you can then find the webhook by
its metadata.

## 3. Your questions

1. **Q7:** Row writes exist now, in the V4 data API (P1-G1). We won't
   build a parameterised SQL endpoint.
2. **Q8:** Yes, on the data API (P1-G2).
3. **Q15:** Yes. Deliveries count against the **organisation's** plan
   rate (Personal 60/min, Team 300/min), which every webhook in the
   organisation shares, yours and the customer's own. A backlog of N
   events takes about N ÷ the rate minutes to drain. Nothing is dropped.
4. **Q13:** Yes, it's a V3 feature (`docs/free-tier.md`): a Free project
   with no clients for a week is paused, and after 90 days archived.
   - **Data API:** for both, it answers `503 project_resuming` with
     `Retry-After: 10`, and the request wakes the project.
   - **Management API:** calls that need the project's database (the SQL
     endpoint, table rows, webhook changes) now answer the same way
     (`503 project_resuming`, `Retry-After: 10`). For an archived project
     they answer `503 project_restoring` with `Retry-After: 60`, since
     restoring takes a minute or two. In both cases the call queues the
     wake.
   - **Reads** of Xenibase's own data (webhook get and list) still answer
     normally.
   - So the joint doc can keep C6 as written.
5. **Q21:** The public contracts covered by the deprecation policy are:
   - `api/openapi.yaml`, the management API;
   - the data API, as in `docs/backend-services.md`, plus each project's
     `/data/v1/openapi.json`;
   - auth (`/auth/v1`) and storage (`/storage/v1`), as in
     `docs/backend-services.md`;
   - `docs/webhooks.md`;
   - `docs/cli.md`;
   - the error codes in `docs/errors.md`.

   The policy is at the end of `docs/errors.md`: within a major version
   we only add fields and codes, and any removal or change of meaning is
   announced in `CHANGELOG.md` and kept working for at least 6 months.
   Auth and storage have no OpenAPI file yet; their docs are the
   contract. V3 billing has no outbound events to build on (see P4).
6. **Q20:** Yes. Xenibase is installed per operator (`docs/install.md`),
   so let each connection set its own management base URL and API
   domain. Our hosted install's URLs come with S1.
7. **Q23:**
   - `primary_key` is now on **every** change event of a table that has
     one, not only on truncated events.
   - For a truncated event it is always enough to fetch the row: `GET
     /data/v1/{table}/{key}` for a single-column key, or a `where`
     filter on each key column for a composite one.
   - A table without a primary key has no `primary_key`, and a truncated
     event from it can't be refetched. Your trigger setup could warn
     about such tables.
   - Each event now also carries `project_id` (see Q28).
8. **Q22:** Yes, now. `POST …/rotate-secret` with
   `{"overlap_seconds": N}` (up to 86,400) signs every delivery with
   both secrets for N seconds (`t=…,v1=<new>,v1=<old>`). Both the answer
   and the webhook show `previous_secret_expires_at`. Your verifier
   should accept the delivery if any `v1` matches; our Go and Node
   snippets in `docs/webhooks.md` do. With no body the old secret stops
   at once, as before.
9. **Q24:** `old_record` is always the **whole** row before the change
   (`to_jsonb(OLD)`), every column. A webhook can also be limited to
   changes in certain columns (`columns`).
10. **Q25:** Send `Authorization: Bearer xb_…`. The OpenAPI file now
    declares it as `bearerAuth` (beside `sessionCookie`). Yes, token
    calls skip the CSRF header. Only browser sessions need it.
11. **Q26:** `write`, and no `confirm`. Every webhook call, reads
    included, needs `write`.
12. **Q27:** Yes, names are unique within a project. Find yours by
    `metadata` rather than by name, because a person may rename it.
13. **Q28:** No. `project` is the project's **database name** (`p_…`), as
    in its connection strings. The new `project_id` field is the UUID
    the API uses.

## 4. Later phases

- **C1:** The V4 data API, auth and storage are published, as listed in
  Q21. Paused projects (V3) are covered in `docs/free-tier.md` and Q13.
  **Billing events:** Xenibase doesn't emit any yet (`docs/billing.md`
  describes invoices and dunning inside Xenibase). We'd like to work out
  the event list with you under P4.
- **P2-G1:** Done. `docs/oauth-apps.md`: authorization code with PKCE
  (S256, required), a consent page where the person picks the
  organisation, projects, scopes and expiry, and an exchange that issues
  an ordinary API token (`created_via: oauth`, shown on the Account page
  as a connected app, revocable there or through `POST /oauth/revoke`).
  Register Taskiem's client with `POST /api/v1/admin/oauth/clients`.
  Pasting a token still works.
- **P3-G1:** It exists (P1-G1).
- **P3-G2:** It exists. `POST /api/v1/projects` works with a token that
  has the `write` scope and is not restricted to projects, within the
  organisation's quota (`409 quota_exceeded` past it). A restricted
  token can't create projects.
- **P4, contract tests:** Yes. Send us your `xenibase@1` suite and we'll
  run it in CI on changes to the contracts above. Our own contract checks
  are `TestWebhookIntegratorContract`, `TestWebhookFixtures`,
  `TestDataAPIIdempotencyKey` and `TestPausedProjectAnswersResuming`.
- **P4, integrators' note:** It's in `docs/webhooks.md`: create webhooks
  through the API with a project-restricted token, and deliveries count
  against the organisation's rate.
- **P4, operations workflows:** Let's start with a short list of the
  processes (dunning, onboarding, alerts) and agree the events after
  that. Nothing is emitted today.
- **P4, Taskiem's database on a dedicated instance:**
  - **Creating roles:** not by the owner (its roles are `NOSUPERUSER
    NOCREATEDB NOCREATEROLE NOBYPASSRLS`). Since 17 October 2026 a
    platform admin can create NOLOGIN integrator roles on a **dedicated**
    project for you (`POST /api/v1/admin/projects/{project_id}/roles`,
    `{"roles": ["taskiem_app", "taskiem_dispatch"]}`), granted to the
    owner `WITH ADMIN OPTION`; your migrations then find them and skip
    creating them. Moves recreate them; the isolation audit expects them;
    demotion to the shared tier stays refused while they exist.
  - **`SECURITY DEFINER` functions:** yes, as the owner.
  - **Forced row-level security:** yes, as the owner of the tables.
  - **`LISTEN/NOTIFY`:** yes, over a direct or session-pooler
    connection. The transaction pooler can't carry it.
  - **Partitioning:** yes.
  - **A migration user that owns the schema:** yes, the project's owner
    role owns everything it creates.

  So the execution store is feasible on a dedicated project with the
  roles made for you. C11 can be restated as that check.

## 5. Joint doc points

- **C6, resuming projects:** confirmed. See Q13 for the codes.
- **C9, parameterised SQL:** reword it to use the data API's row writes
  (P1-G1). There will be no parameterised SQL endpoint.
- **C10, idempotency keys:** they exist on the data API. Reword it to
  depend on them.
- **C11, execution store:** a feasibility check, per section 4.
- **C4, Q1, Q16, shared building blocks:** a decision for people. We
  suggest a call to choose, area by area, whether to adopt or align
  (payments, messaging, metering and plans, the SSRF guard).
