# Backend services

Backend services (V4) let an app talk to its project over HTTPS, straight
from a browser or a phone, without a server of its own: the data API, auth,
storage and realtime. This page covers what is in place now: the edge gateway, project API
hostnames, API keys and the per-request roles and claims (V4-M28), and the
data API's reads (V4-M29). Writes, auth, storage and realtime arrive in the
milestones after them.

## How a request is served

```
app ──HTTPS──▶ <ref>.<domain> ──▶ xenibase-edge (per region)
                                    │ key, token, CORS, rate limit
                                    ▼
                        transaction pooler ──▶ the project's database
                        BEGIN; SET LOCAL ROLE <db>_anon|_user|_service;
                        set_config('xb.claims', …, true); …; COMMIT
```

- Each project with backend services has a **reference**, eight letters and
  digits starting with a letter (`k7f3m2q9`), and is served at
  `https://<ref>.<domain>`, where the domain is `XENIBASE_API_DOMAIN`.
- **xenibase-edge** resolves exactly one project per request from the
  hostname, checks the API key against that project's keys only, and runs
  the request in one transaction through the transaction pooler. It logs
  in as the project's `<db>_edge` role, which owns nothing and can only
  `SET ROLE` to the three request roles.
- The claims of the request are set for that transaction only, so nothing
  carries over to the next request on the same pooled connection.

## Keys

| Key | Prefix | Used from | Requests run as |
| --- | --- | --- | --- |
| Publishable | `xb_pub_` | Browsers and mobile apps; meant to be embedded | `<db>_anon`, or `<db>_user` with a signed-in user's token |
| Secret | `xb_sec_` | Servers only | `<db>_service` (bypasses row-level security) |

- Send the key in the `apikey` header. A signed-in user's access token goes
  in `Authorization: Bearer …`; it is an ES256 JWT for this project
  (`aud` is the reference), verified with the project's public keys.
- Keys are stored hashed. The publishable key stays visible in the
  dashboard; a **secret key is shown once**, when it is made.
- A secret key sent from a page (a request with an `Origin` header) is
  refused unless the project allows it.
- Make a second key and move your apps to it before revoking the first:
  revoking takes effect at the edge within seconds.

## Turning it on

Project → Settings → **API** → **Enable backend services**, or
`xenibase services enable <project>`. Enabling:

1. creates `<db>_edge`, `<db>_anon`, `<db>_user` and `<db>_service` and lets
   the pooler accept the edge login;
2. adds the `xb_auth`, `xb_storage` and `xb_realtime` schemas, owned by
   the platform, with `xb_auth.uid()`, `role()`, `claims()` and
   `claim(text)` for your row-level security policies;
3. grants the three request roles use of what the owner creates in
   `public` (row-level security decides which rows; anon and user keep
   privileges only on what row-level security protects, below);
4. makes the first publishable and secret keys and the project's signing
   key.

```sql
ALTER TABLE todos ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_todos ON todos FOR ALL TO "<db>_user"
  USING (owner_id = xb_auth.uid()) WITH CHECK (owner_id = xb_auth.uid());
```

Turning it off revokes the keys and switches the edge login off. The
`xb_*` schemas and their data stay, so turning it on again restores
everything except the keys, which are new.

The `xb_*` schemas are part of the database: backups, restores and
branches carry them. After a restore or a move, Xenibase takes them back from
the project's owner (a restore recreates them as the owner's) and re-applies
the request roles' grants, within seconds. A promotion or demotion creates
the request roles on the new instance before copying.

Check that it works:

```sh
curl -H "apikey: xb_pub_…" https://k7f3m2q9.api.xenibase.ng/data/v1/health
# {"status":"ok","project":"k7f3m2q9","role":"anon","region":"ng-lagos"}
```

## Reading data

`https://<ref>.<domain>/data/v1/<table>` reads a table or view in the
exposed schemas (`public` unless you choose others). Name a table in another
exposed schema as `schema.table`.

```sh
curl -G https://k7f3m2q9.api.xenibase.ng/data/v1/todos \
  -H "apikey: xb_pub_…" -H "Authorization: Bearer $ACCESS_TOKEN" \
  --data-urlencode "select=id,title,done,owner:profiles(name)" \
  --data-urlencode "where=done:eq:false" \
  --data-urlencode "order=created_at:desc" --data-urlencode "limit=20"
# {"data":[{"id":3,"title":"c","done":false,"owner":{"name":"Ada"}}, …],"next_cursor":"eyJr…"}
```

| Parameter | |
| --- | --- |
| `select` | Columns (`*` by default), `alias:column`, JSON paths (`meta->plan`), and related rows through foreign keys: `author(name)` gives an object (or `null`) when this table points at it, and `comments(id,body)` an array when it points at this table. Name a relation by its table (or by the foreign key column without `_id`); when two foreign keys reach the same table, say which: `editor:profiles!editor_id(name)`. Up to 3 levels; arrays hold up to 1,000 rows. |
| `where` | `column:operator:value`, repeatable (all must hold). Operators: `eq`, `neq`, `lt`, `lte`, `gt`, `gte`, `in` (`a,b,"c,d"`), `like`, `ilike` (`%` wildcards), `is` (`null`, `true`, `false`), `contains`, `contained_by` (arrays, jsonb), `search` (full text, `websearch_to_tsquery`). JSON paths compare as text: `where=meta->tag:eq:work`. |
| `or` | One group of conditions any of which may hold: `or=status:eq:draft,owner_id:eq:…`. Repeatable; each group must hold. |
| `order` | `column:asc` or `column:desc`, comma-separated. The primary key is added so pages are stable. |
| `limit` | Default 100, at most 1,000. |
| `cursor` | The `next_cursor` of the previous page (keyset pagination, for any depth). `offset` also works, up to 10,000. |
| `count` | `exact` (capped at 2 seconds, `null` if it ran out) or `estimated` (the planner's). |

- **One row:** `GET /data/v1/todos/42` (single-column primary keys) returns
  `{"data":{…}}`, or `404` when it doesn't exist or the caller can't see it.
- **Complex queries:** `POST /data/v1/todos/query` with
  `{"select":"…","where":{"or":[{"column":"done","op":"eq","value":true},{"not":{…}}]},"order":"title:desc","limit":50}`;
  `and`, `or` and `not` nest.
- **The project's OpenAPI document:** `GET /data/v1/openapi.json` with the
  secret key, generated from the current schema.

### Row-level security comes first

With the publishable key, requests run as `<db>_anon` (or `<db>_user` with a
signed-in user's token), and **a table without row-level security is not
readable**: the API answers `403 rls_required`. Enable it and write policies,
or, for data anyone may read, list the table under **Public tables**.
Views must run as the caller (`CREATE VIEW … WITH (security_invoker = true)`)
so the underlying tables' policies apply; embedded tables are checked the
same way. The secret key bypasses row-level security entirely.

A table without row-level security is out of reach through views and
functions too. While it stays that way, `<db>_anon` and `<db>_user` hold no
privileges on it (on a public table, `SELECT` only): Xenibase takes them back
within a couple of seconds of the table appearing or losing row-level
security, and gives exactly those back once it has it, so the usual order
(create the table, then enable row-level security and add policies) works.
`xb_auth.withheld` lists what is held back. The same goes for materialized
views and views that run as their owner. A view that runs as the caller but
reads such a table answers `403 rls_required` naming it, and so does a
function that runs as the caller while anon or user can still reach one
(after a grant made by hand, say).

```sql
ALTER TABLE posts ENABLE ROW LEVEL SECURITY;
CREATE POLICY readable ON posts FOR SELECT
  USING (published OR author_id = xb_auth.uid());
```

Column privileges hold too. To hide a column from anonymous callers, take
away the table-level `SELECT` and grant the columns they may read; a
column-level `REVOKE` alone does nothing while the table-level grant stands:

```sql
REVOKE SELECT ON profiles FROM "<db>_anon";
GRANT SELECT (id, name, avatar_url) ON profiles TO "<db>_anon";
```

Do the same for `"<db>_user"` to hide it from signed-in users. Xenibase keeps
these grants when it re-applies the request roles' grants after a move or
restore. `select=*` then answers `403 permission_denied`: name the columns
(`select=id,name`). The security advisor's `secret_column` finding gives the
statements for the table.

### Guardrails

- Each request has its statement timeout (8 s by default), at most 30
  filters and 3 levels of relations, and a 10 MB result.
- Every read is planned (`EXPLAIN`) as the caller, with its own values, and
  refused with `400 query_too_expensive` when its estimated cost is over the
  project's limit (1,000,000 by default: a scan of a few million rows). Add an
  index or a filter, or raise the limit in the settings. A set-returning
  function's call is checked the same way, and `count=exact` over the limit
  is not run (`"count": null`, as when it times out).
- Schema changes are picked up within about two seconds, with no restart.

### Errors

```json
{"error":{"code":"unknown_column","message":"todos has no column \"nope\"","request_id":"req_…"}}
```

| Status | Codes |
| --- | --- |
| 400 | `invalid_filter`, `invalid_select`, `invalid_value`, `unknown_column`, `unknown_relation`, `ambiguous_relation`, `invalid_cursor`, `invalid_limit`, `query_too_expensive`, `filter_required`, `too_many_rows`, `invalid_body`, `no_matching_function`, `ambiguous_function`, `invalid_idempotency_key`, `idempotency_needs_user`, … |
| 401 | `key_required`, `invalid_key`, `invalid_token` |
| 403 | `rls_required`, `permission_denied`, `secret_key_in_browser`, `origin_not_allowed`, `project_suspended` |
| 404 | `unknown_table`, `unknown_function`, `not_found` |
| 409, 422 | `unique_violation`, `foreign_key_violation`; `not_null_violation`, `check_violation`, `idempotency_key_reused` |
| 405 | `method_not_allowed`, `volatile_function` |
| 413 | `result_too_large` |
| 429 | `rate_limited` |
| 503, 504 | `project_resuming`, `database_unavailable`, `retry`, `starting`; `statement_timeout` |

[errors.md](errors.md) lists every code with whether a retry can help.

## Writing data

Writes run as the caller's role in one transaction, so row-level security's
`USING` and `WITH CHECK` decide what each caller may change. With the
publishable key, a table is writable only with row-level security on (a
view only with `security_invoker`); **public tables are readable, never
writable, without it**.

```sh
# Insert one row or a list (up to 1,000); the new rows come back.
curl -X POST https://k7f3m2q9.api.xenibase.ng/data/v1/todos \
  -H "apikey: xb_pub_…" -H "Authorization: Bearer $ACCESS_TOKEN" \
  -d '{"owner_id":"…","title":"Buy milk"}'
# {"affected":1,"data":[{"id":7,"owner_id":"…","title":"Buy milk","done":false}]}

# Update the rows a filter matches, or one row by key.
curl -X PATCH ".../data/v1/todos?where=done:eq:false" -d '{"done":true}'
curl -X PATCH ".../data/v1/todos/7" -d '{"title":"Buy oat milk"}'

# Delete.
curl -X DELETE ".../data/v1/todos?where=title:eq:Buy%20milk"
curl -X DELETE ".../data/v1/todos/7"
```

| Parameter | |
| --- | --- |
| `on_conflict` | Upsert: the columns of a unique constraint (`on_conflict=email`). Conflicting rows are updated with the sent columns; with `resolution=ignore` they are left alone. |
| `select` | Which columns of the written rows come back (the read API's `select`, without relations). |
| `return` | `representation` (default) or `minimal` (only `affected`; the rows aren't read back, so an `INSERT`-only policy is enough, as for a contact form). |
| `where`, `or` | Which rows an update or delete changes, as for reads. **An update or delete needs a filter or a key**: `400 filter_required` otherwise. |
| `max_affected` | Refuse (`400 too_many_rows`, nothing changed) when more rows would change; 1,000 by default, at most 100,000. |

`PUT` isn't used: insert or upsert with `POST`, change with `PATCH`.

### Retrying writes safely: Idempotency-Key

When a write times out you can't tell whether it happened. Send an
`Idempotency-Key` header (1 to 255 printable characters; a UUID is a good
choice) on `POST`, `PATCH` or `DELETE` of a table, on a batch, or on a
`POST` to a function, and repeat it with the retry:

```sh
curl -X POST https://k7f3m2q9.api.xenibase.ng/data/v1/orders \
  -H "apikey: xb_sec_…" -H "Idempotency-Key: 6f1c0e4a-…" -d '{"item":"tea"}'
```

- A repeat **within 24 hours** gets the first answer (same status and
  body) with `Idempotent-Replayed: true`, and writes nothing.
- The same key with a **different request** (method, path, parameters or
  body) gets `422 idempotency_key_reused` and writes nothing.
- The key is recorded in the write's own transaction, so it exists exactly
  when the write committed. A write that failed (a constraint, a timeout,
  a lost connection before the commit) left no key: the retry runs it.
- A repeat sent while the first is still running waits for it, then gets
  its answer.
- Keys belong to the caller: the secret key's, or a signed-in user's. The
  publishable key **without** a user can't use them (`400
  idempotency_needs_user`), since every such caller would share them.
- Answers are kept in the project's database (`xb_auth.idempotency`,
  which your roles can't read) and removed after a day.

### Batches

`POST /data/v1/batch` runs up to 50 writes in one transaction: all of them
happen or none do.

```json
{"operations":[
  {"op":"insert","table":"orders","rows":[{"item":"tea"}]},
  {"op":"upsert","table":"stock","rows":{"item":"tea","n":9},"on_conflict":["item"]},
  {"op":"update","table":"carts","where":{"column":"id","op":"eq","value":4},"set":{"closed":true}},
  {"op":"delete","table":"cart_items","key":4,"return":"minimal"}]}
```

The answer is `{"results":[…]}`, one per operation. When one fails, the error
names it (`details.operation`, counted from 0) and says nothing was changed.

## Caching anonymous reads

Reads that every visitor makes the same way (a product list, a
storefront's categories) can be served from the edge instead of the
database. List the tables and functions, with how long a cached answer may
be served, under Settings → API → **Cache anonymous reads**, or in the
settings' `cache_ttl_seconds`:

```json
{"settings": {"cache_ttl_seconds": {"public.products": 60, "rpc.search_products": 30}}}
```

Tables are `schema.table`; functions are `rpc.name` (in `public`) or
`rpc.schema.name`, and must be `STABLE` or `IMMUTABLE` (called with `GET`).
At most 3,600 seconds and 100 entries.

- **What is cached:** `GET`s with the publishable key and **no** user
  token (so every caller is `anon` and the same policies apply), unless the
  request sends `Read-Replica: primary`. Never a signed-in user's read, the
  secret key, writes, storage, auth or realtime.
- **The key** is the project, the path and its query with the parameters
  sorted (the `apikey` parameter left out), the project's configuration
  version and its schema's fingerprint, so changing the settings or the
  schema starts afresh. The fingerprint includes policies, grants and
  row-level security: closing a policy, revoking a grant or turning
  row-level security off empties the project's cache within about two
  seconds, whatever the TTL.
- **Headers:** `X-Cache: HIT` or `MISS`, `Age`, and
  `Cache-Control: public, max-age=<seconds left>` with
  `Vary: Authorization, Read-Replica`, so a CDN in front may cache too
  (and keeps signed-in users' requests apart).
- **A TTL is a staleness budget.** A write through this edge's data API
  (insert, update, delete, upsert, batch, or a function called with `POST`)
  drops the entries it could have changed before it answers: those of the
  table, of reads that embed it, and every function result. With realtime
  capture on a table, its changes drop its entries on every edge listening
  to the project. Anything else (SQL, scheduled jobs, a write through
  another edge) shows within the TTL.
- **Billing:** cached answers still count as requests and transfer.
- Each edge process keeps its own cache, at most 64 MB by default
  (`XENIBASE_EDGE_CACHE_MB`); the least recently used entries go first, and
  one answer may take at most an eighth of it.

## Functions

`/data/v1/rpc/<function>` calls a function in the exposed schemas as the
caller, with named arguments:

```sh
curl -X POST .../data/v1/rpc/close_cart -d '{"cart_id": 4}'
curl ".../data/v1/rpc/search_todos?q=milk&limit=5"   # STABLE or IMMUTABLE only
```

- `POST` takes the arguments as a JSON object; `GET` takes them as query
  parameters and works only for `STABLE` or `IMMUTABLE` functions
  (`405 volatile_function` otherwise).
- A scalar result comes back as `{"data": 42}`; a set or a table as a list,
  with the read API's `select`, `where`, `order` and `limit` applied to its
  rows.
- Overloads are chosen by the argument names given; `400
  ambiguous_function` when more than one fits.
- `SECURITY DEFINER` functions run as their owner and skip row-level
  security: the security advisor lists them.
- Functions an extension installed (`pg_stat_statements_reset`, PostGIS's)
  are not served (`404 unknown_function`), and the request roles are not
  granted them, so one an extension keeps from `PUBLIC` stays the
  platform's. Wrap one in a function of your own to call it through the API.

## Generated types

Typed rows, inserts and updates for every table, view and function the data
API exposes:

```sh
xenibase gen types --lang ts --project my-app > src/database.types.ts
xenibase gen types --lang dart --project my-app -o lib/database_types.dart
xenibase gen types --lang go --project my-app --package db -o db/types.go
```

- **TypeScript:** a `Database` interface (`Database["public"]["Tables"]["todos"]["Row" | "Insert" | "Update"]`, `Views`, `Functions` with `Args` and `Returns`) and `Tables<"todos">`-style helpers. Columns with a default or that accept null are optional in `Insert`; generated columns are left out.
- **Dart:** a class per table and view with `fromJson` and `toJson`.
- **Go:** a struct per table and view, `…Insert` and `…Update` structs (pointers with `omitempty` for what may be left out) and a `…Table` constant per name.

The same files download from Project → Settings → API → Generate types, or
`GET /api/v1/projects/{id}/services/types?lang=ts|dart|go`.

Table and column names appear in the files only as escaped strings; field and
class names keep their letters and digits (`price$usd` becomes `PriceUsd`, a
Dart field named `class` becomes `class_`, and a second name that comes out
the same gets a number). Go's JSON tags can't hold some characters (quotes,
a comma, a backslash, control characters), so Go generation refuses a column
with one, naming it (`400`); rename the column or use TypeScript or Dart.

## Security advisor

Project → Settings → API → Security advisor (or
`GET /api/v1/projects/{id}/services/advisor`) checks the exposed schemas:

| Level | Finding |
| --- | --- |
| danger | A table without row-level security (`rls_disabled`); a write policy open to anon (or `PUBLIC`) with no condition (`policy_allows_everything`). |
| warn | `SECURITY DEFINER` functions (`security_definer_function`); views that don't run as the caller (`view_not_invoker`); views that run as the caller but read a table without row-level security (`view_reads_unprotected`); columns named like secrets (password, token, secret…) readable by anon or user (`secret_column`). |
| info | Public tables; row-level security with no policies (only the secret key gets in); read policies open to everyone. |

Each finding says what to do and, where one statement fixes it, gives it.

**The policy helper:** in the Table Editor, **Policies** on a table turns on
row-level security and writes the policies for one of three templates
(owner only, members of an organisation through a membership table, public
read with owner writes), showing the SQL before it runs.

## The API page

Project → Settings → **API**, once services are on:

- **Quick start:** the project URL and publishable key filled into a first
  program in TypeScript, Dart and Go (install, `createClient`, a read,
  sign-in with a phone code), with a link to the generated types. Hide it
  and it stays hidden for you in that browser.
- **API docs:** each exposed table, view and function, from the same
  catalog as the generated types: its columns (types, nullability,
  defaults, identity, generated and enum values), whether row-level
  security is on and its policies (in the request roles' names: anon,
  user, service), what anon and a signed-in user may do, and examples of a
  read (with a filter and the relations its foreign keys reach), insert,
  update, delete, upsert and function call in curl, TypeScript, Dart and Go.
  Examples only ever hold the publishable key. **Try it** puts the request
  in the request explorer below. The same catalog is at
  `GET /api/v1/projects/{id}/services/catalog`.
- **Usage this month:** requests, transfer, monthly active users, SMS and
  WhatsApp codes, file storage and downloads, image transforms and
  realtime minutes and messages, for this project and for the whole
  organisation, against the plan's allowance and its hard limits (which
  the organisation's projects share). Owners and billing members also see
  the month's charges so far that fall to this project: its own lines in
  full, and of each allowance's overage the share its usage is of the
  organisation's (`GET /api/v1/projects/{id}/services/usage`).

## Request explorer

Project → Settings → API → Request explorer sends a data API request as
anon, as a user (by their id, which becomes the token's `sub`) or as the
service role and shows the answer, without handing out a key or token
(`POST /api/v1/projects/{id}/services/explore`; developers and up). Writes
made there are real.

## Auth

Users live in the project's own database, in `xb_auth` (V4 §4.2), so they
move with it: backups, restores, branches and promotions keep them, and
data residency covers them. Only xenibase-edge reads the auth tables; your
SQL sees the safe view `xb_auth.user_profiles` (no password hashes,
tokens or codes) and may reference `xb_auth.users(id)` from its own
tables:

```sql
CREATE TABLE todos (
  id bigserial PRIMARY KEY,
  owner_id uuid NOT NULL DEFAULT xb_auth.uid() REFERENCES xb_auth.users (id) ON DELETE CASCADE,
  title text NOT NULL
);
ALTER TABLE todos ENABLE ROW LEVEL SECURITY;
CREATE POLICY own ON todos FOR ALL TO "<db>_user"
  USING (owner_id = xb_auth.uid()) WITH CHECK (owner_id = xb_auth.uid());
```

### Signing up and in

All at `https://<ref>.<domain>/auth/v1/`, with the publishable key:

```sh
curl -X POST .../auth/v1/signup -H "apikey: xb_pub_…" \
  -d '{"email":"ada@example.com","password":"…","data":{"name":"Ada"},"redirect_to":"https://app.example.com/welcome"}'
# {"confirmation_sent":true}       (or a session, when confirmation is off)

curl -X POST .../auth/v1/signin/password -H "apikey: xb_pub_…" -d '{"email":"ada@example.com","password":"…"}'
# {"access_token":"eyJ…","token_type":"bearer","expires_in":3600,"expires_at":…,"refresh_token":"…","user":{…}}
```

| Endpoint | |
| --- | --- |
| `POST signup` | Email and password (argon2id). With **Confirm email addresses** on (the default) the user gets a link and a 6-digit code and can't sign in with the password until they use one; the answer is the same whether or not the address was new. Signing up again before confirming replaces the password; confirming keeps only the password of the sign-up whose code was used, so someone who signed up with the address first can't keep theirs. |
| `POST signin/password` (or `POST token?grant_type=password`) | A session. Five wrong passwords in a row lock the user for a minute, doubling with each further failure (`429 user_locked`). |
| `POST signin/otp` | A magic link and a code by email (`create_user: false` to only sign in existing users). Proving an unconfirmed address this way (or by a reset) clears any password set on it before, so nobody can claim an address ahead of its owner. |
| `POST verify` | `{"type":"signup"\|"magiclink"\|"email"\|"recovery"\|"invite"\|"email_change","email":"…","token":"123456"}`, or `{"type":…,"token_hash":"…"}` with the link's token: a session. A code works once, for 10 minutes (invitations a day), and 5 wrong tries use it up. |
| `GET verify?token=…&type=…&redirect_to=…` | The link in the email (no key needed): redirects to `redirect_to` with the session in the fragment (`#access_token=…&refresh_token=…`), or `#error=access_denied&error_code=otp_expired`. |
| `POST resend` | Another confirmation email (`{"type":"signup","email":…}`). |
| `POST recover` | A password-reset link and code; verifying it signs the user in to set a new password. |
| `POST token?grant_type=refresh_token` | `{"refresh_token":"…"}`: new tokens; the refresh token is single use. |
| `POST signout?scope=local\|others\|global` | With the access token: this session, the others, or every session. |
| `GET user`, `PATCH user` | The signed-in user with their `identities` and `factors`; change the password, `data` (merged into the user's metadata), the email (confirmed by a link sent to the new address) or the `phone` (confirmed by a code to the new number). A user with a verified factor changes the password or email only at `aal2` (`401 insufficient_aal`). |
| `GET settings` | Which methods are on and the password rules, for your sign-in form. |
| `GET .well-known/jwks.json` | The public keys (no key needed). |

Links go only to the **site URL** (and pages under it) or the exact
**redirect URLs** you list; anything else is `400 redirect_not_allowed`.
With wildcards allowed, a redirect URL may be a pattern, matched part by
part: `*` in the host stands for one label or part of one
(`https://*.example.com` allows `https://a.example.com` but not
`https://a.b.example.com`), `:*` for any port, and in the path `*` is
anything but `/` and `**` anything. A query or fragment is allowed only
when the pattern's path ends in `**` or the pattern has one.

### Phone: SMS and WhatsApp codes

Turn the channels on in Authentication → Phone. Numbers may be written the
local way (`0803 123 4567`) or internationally (`+2348031234567`); they are
stored in E.164.

```sh
curl -X POST .../auth/v1/signin/otp -H "apikey: xb_pub_…" -d '{"phone":"08031234567","channel":"whatsapp"}'
curl -X POST .../auth/v1/verify -H "apikey: xb_pub_…" -d '{"type":"sms","phone":"+2348031234567","token":"123456"}'
```

| Endpoint | |
| --- | --- |
| `POST signin/otp` with `phone` and `channel` (`sms` default, or `whatsapp`) | A code to the number, creating the user when sign-ups are on (`create_user: false` to only sign in). |
| `POST signup` with `phone` and `password` | A phone and password account; with **Confirm numbers at sign-up** on (the default) a code goes to the number first. |
| `POST signin/password` with `phone` | Signs in with the number and password. |
| `POST verify` | `{"type":"sms","phone":…,"token":…}` (also `"whatsapp"`), or `"phone_change"` for a new number. |
| `PATCH user` with `phone` (and `channel`) | Sends a code to the new number. |
| `POST resend` with `{"type":"sms","phone":…}` | Another code (only to an existing user). |

**Who sends.** By default, Xenibase's own accounts: Termii for SMS, through
its DND route so numbers registered as do-not-disturb still get codes,
and Xenibase's WhatsApp Business number with an approved authentication
template. Each message is metered (`messages_sms`, `messages_whatsapp`)
and its cost shown under Authentication → Phone. Free projects bring their
own provider unless the install allows otherwise. A project can use its
own **Termii, Africa's Talking or Twilio** account for SMS and its own
**WhatsApp Cloud API** number (or Twilio) for WhatsApp; Xenibase doesn't bill
those messages. A **send-message hook** (below) replaces sending entirely.

**SMS pumping.** Paid codes invite fraud: someone requests codes to numbers
they profit from. Four limits stop it:

- **Countries**: numbers must be in the project's countries (Nigeria only
  by default; `403 phone_country_not_allowed` otherwise).
- **Per number**: one code a minute from the edge, and at most 5 an hour at
  xenibase-server (`429 over_sms_send_rate_limit`).
- **Daily cap**: 200 codes a day per project by default. At the cap, codes
  stop (`429`, "daily limit") and the project's admins are emailed; they
  are also emailed when a quarter of the cap goes out in one hour.
- **Captcha** (below) on code requests.

### OAuth: Google, Apple, GitHub, Facebook, Microsoft

Each project uses its own OAuth app. In Authentication → Providers, turn a
provider on with its client ID and secret (Apple: the Services ID, team
ID, key ID and the `.p8` key) and register the **callback URL** shown
there, `https://<ref>.<domain>/auth/v1/callback`, with the provider.

With PKCE (what Supabase's clients do by default):

1. The app sends the browser to
   `GET /auth/v1/authorize?provider=google&redirect_to=<allowed URL>&code_challenge=…&code_challenge_method=s256`
   (no key needed).
2. After the provider, the browser returns to `redirect_to?code=…`.
3. The app exchanges it: `POST /auth/v1/token?grant_type=pkce` with
   `{"auth_code":…,"code_verifier":…}`. The code works once, for 5
   minutes.

Without a `code_challenge` the session comes back in the redirect's
fragment instead. A provider's verified email joins the user who has that
email (and clears a password someone set on it unconfirmed); an
unverified one never does. Microsoft's email isn't treated as verified.

### Anonymous users and identities

With **Anonymous sign-in** on, `POST /auth/v1/signup` with an empty body
returns a session for a new user with `is_anonymous: true` (the claim is
in the token, and `xb_auth.is_anonymous()` reads it in policies). The
user becomes permanent when they add an email or phone (`PATCH user`, then
verify) or link a provider.

A signed-in user can link a provider with
`GET /auth/v1/user/identities/authorize?provider=…&redirect_to=…` (with
their token; the answer is `{"url":…}` to open) and unlink one with
`DELETE /auth/v1/user/identities/{id}`; the last way to sign in can't be
unlinked. Both can be turned off.

### Multi-factor authentication

Users enrol an authenticator app (TOTP) or, when allowed, a phone:

| Endpoint (with the user's token) | |
| --- | --- |
| `POST factors` `{"factor_type":"totp","friendly_name":…}` | The secret and an `otpauth://` URI for a QR code. Phone: `{"factor_type":"phone","phone":…}`. A user who already has a verified factor enrols another only at `aal2` (`403 insufficient_aal`). |
| `POST factors/{id}/challenge` | A challenge (a phone factor gets a code). |
| `POST factors/{id}/verify` `{"challenge_id":…,"code":…}` | New tokens at `aal2`; the factor is verified. A new factor is verified only when it is the user's first or the session is at `aal2`. Five wrong codes in a row, across challenges, lock the factor for a minute, doubling with each further one up to a day (`429 mfa_factor_locked`). |
| `DELETE factors/{id}` | Removes a factor (a verified one needs `aal2`). |

`/mfa/enroll`, `/mfa/challenge` and `/mfa/verify` (with `factor_id` in the
body) do the same. The **policy** (Authentication → MFA and captcha) is
optional (users choose), required (the data API answers only `aal2`
tokens: `403 mfa_required`), or required by claim for users whose
`app_metadata.mfa_required` is true. Policies can check
`xb_auth.aal() = 'aal2'` themselves.

### Hooks

| Hook | | |
| --- | --- | --- |
| **Custom claims** | A Postgres function `schema.name(event jsonb) returns jsonb` | Called when a token is issued with `{"user_id","claims","authentication_method"}`; return `{"claims":{…}}`. Claims the edge sets (`sub`, `role`, `aud`, `exp`, `iat`, `session_id`, `aal`, `amr`, `is_anonymous`) can't be changed. |
| **Before sign-up** | A Postgres function, or a webhook | Called with `{"user","method","ip"}`; `{"decision":"reject","message":…}` (or a webhook's 4xx) refuses with `403 signup_rejected`. A webhook that doesn't answer in 3 seconds lets the sign-up through. |
| **After sign-up**, **after sign-in** | Webhooks | `{"type":"after_signup","data":{"user":…,"method":…}}`, queued and retried like database webhooks. |
| **Send message** | A webhook | Receives every email and code (`{"type":"send_message","channel","kind","to","body","code","link"}`) instead of Xenibase sending it. |

Postgres hooks run in their own transaction, with a 2-second timeout, as
the project's **hook role** `<db>_auth_hook` (a login of its own, like the
request roles: code that runs `RESET ROLE` stays that role), which has no privileges
beyond reading `xb_auth.user_profiles` and the `xb_auth` functions:
grant it what the hook reads.

```sql
GRANT SELECT ON members TO "<db>_auth_hook";
CREATE FUNCTION public.custom_claims(event jsonb) RETURNS jsonb LANGUAGE sql STABLE AS $$
  SELECT jsonb_build_object('claims', (event -> 'claims') || coalesce(
    (SELECT jsonb_build_object('org_id', org_id) FROM members WHERE user_id = (event ->> 'user_id')::uuid), '{}'::jsonb))
$$;
-- then, in a policy: USING (org_id = xb_auth.claim('org_id'))
```

A failing custom-claims hook fails the sign-in (`500`), so test it before
turning it on. Webhooks are signed with the project's **hook secret**
(`Xenibase-Signature`, the same scheme as database webhooks), go through the
organisation's outbound rules, and are listed with their results under
Authentication → Hooks. Only project admins see the secret (developers see
that it is set); an admin can rotate it there.

### Captcha

With captcha on (a Cloudflare Turnstile site key and secret in
Authentication → MFA and captcha), sign-up, password sign-in and code
requests need a Turnstile token, sent as
`"gotrue_meta_security":{"captcha_token":…}` (what Supabase's clients
send) or `"captcha_token"` (`400 captcha_failed` otherwise).

### Tokens and sessions

- The access token is an ES256 JWT signed with the project's key, an hour
  by default (5 minutes to 24 hours): `sub` (the user id), `role: "user"`,
  `aud` (the project ref), `session_id`, `aal`, `amr`, `email`, `phone`,
  `is_anonymous`, `app_metadata`, `user_metadata`, and whatever a
  custom-claims hook adds. Send it as `Authorization: Bearer …`
  with the publishable key: requests run as `<db>_user` and
  `xb_auth.uid()` is its `sub`.
- **Your servers verify tokens with the JWKS endpoint**; there is no shared
  secret. Rotating the key (Authentication → Signing keys, or
  `xenibase auth rotate-key`) signs new tokens with a new key and keeps the
  old one in the JWKS for a day, so nobody is signed out. If a key leaked,
  **Revoke** it there (`POST …/auth/signing-keys/{keyId}/revoke`): it leaves
  the JWKS and the edges at once, so tokens it signed stop working within
  seconds; revoking the active key makes a new one and signs everyone out.
- Refresh tokens rotate on every use. **Presenting a refresh token that was
  already used ends its whole session** (`400 refresh_token_reused`): a
  stolen token is good for one refresh at most, and only until the
  rightful app refreshes. Apps should refresh once at a time.
- Sessions can be limited in length and by inactivity, or to one per user.
  A ban or a deletion takes effect at the next refresh; signing out
  revokes refresh tokens at once, and the access token's lifetime bounds
  the rest.

### Emails

Confirmation, magic-link (with the code), password-reset, invitation and
email-change emails are sent by xenibase-server from the project's
templates (Authentication → Emails: plain text with `{{.Code}}`,
`{{.Link}}`, `{{.Email}}`, `{{.SiteURL}}`, with a preview).

- **Platform email** works out of the box for development, at 30 emails an
  hour per project (`429 over_email_send_rate_limit` past that).
- **Your own SMTP server** (Authentication → Emails) sends from your
  domain without that limit. Its password is encrypted; **Send test**
  checks it before saving. Like webhooks, the server must have a public
  address unless the platform admin allows its host for the organisation
  (its outbound allow-list); a failed test says only that it
  failed, with the reason in the server's log. A provider's `base_url`
  follows the same rule.
- One address gets at most one code a minute, five an hour and twenty a
  day (`429 over_email_send_rate_limit`), counted on requests whether or
  not the address has a user (so the limit doesn't tell who has an
  account); sends are counted in the usage (`message_sends`) and shown
  with failures.

A Flutter sample that signs in with a WhatsApp code and Google and reads
org-scoped rows is in [examples/flutter-auth](examples/flutter-auth/).

### Managing users

- **Dashboard:** Project → Authentication lists users (search by email,
  phone or id), shows a user's sessions and recent activity, and invites,
  bans, unbans, confirms, signs out and deletes them. Developers and up.
- **Admin API**, with the **secret key** (`apikey: xb_sec_…`); see
  [the admin API](#the-admin-api) below.
- **CLI:** `xenibase auth users list|show|invite|ban|unban|signout|delete`,
  `xenibase auth config|set|hooks`.

### The admin API

All under `https://<ref>.<domain>/auth/v1/admin/`, with the secret key, from your
servers.

| Endpoint | |
| --- | --- |
| `GET users?page=&per_page=&q=` | A page of users (`per_page` up to 1,000, 50 by default); `q` searches email, phone and id. Answers `{"users":[…],"total","page","per_page"}`. |
| `POST users` | Creates a user. Body: `email` (required), `password` (optional; it must meet the project's password rules, else `422 weak_password`), `email_confirm` (`true` creates the user already confirmed, so no confirmation email is needed), `user_metadata` and `app_metadata` (JSON objects), `ban_duration`. Answers `201` with the user. |
| `GET users/{id}` | The user with their `identities`. |
| `PATCH users/{id}` | Changes any of the fields above (and `email`). `ban_duration` is a duration such as `"24h"`, or `"none"` to lift a ban. |
| `DELETE users/{id}` | Deletes the user, their sessions and their sign-in methods. |
| `POST users/{id}/signout` | Ends all of the user's sessions. |
| `POST invite` | Invites an email address: creates the user and emails an invitation (24 hours). |
| `POST generate-link` | A sign-in link **and** its code, made without sending anything, for a project that sends its own emails or tests sign-in without an inbox. Described next. |

**`generate-link`.** Body:

| Field | |
| --- | --- |
| `type` | Required: `magiclink`, `invite`, `recovery` or `signup`. |
| `email` | Required. |
| `password` | For `signup` only (it must meet the password rules). |
| `data` | For `signup` and `invite`: the new user's metadata (a JSON object). |
| `redirect_to` | Where the link sends the person after it verifies. **It must be the site URL or one of the project's redirect URLs** (Authentication → *Your app*), else `400 redirect_not_allowed`: the allow-list applies to links you generate as it does to ones the project emails. Leave it out to use the site URL. |

Which types need the user to exist already: **`magiclink` and `recovery` do not
create a user** and answer `404 no such user` when there is none (unlike
`signin/otp`, which creates the user when sign-ups are on). **`invite` and
`signup` create the user.** To test sign-in with an address that has never
signed up, create it first with `POST users` and `email_confirm: true`, or use
`type: "signup"`.

The answer:

```json
{
  "action_link": "https://k7f3m2q9.api.example.com/auth/v1/verify?token=…&type=magiclink&redirect_to=…",
  "email_otp": "482913",
  "hashed_token": "…",
  "verification_type": "magiclink",
  "redirect_to": "https://app.example.com/welcome",
  "user": { "id": "…", "email": "ada@example.com", … }
}
```

`action_link` is the link an email would carry (visiting it signs the person in
and redirects with the session in the fragment); `email_otp` is the six-digit
code; `hashed_token` is the link's token. In a test, skip the browser and trade
the code for a session:

```sh
# 1. a confirmed user, 2. a code for them, 3. a session from the code
curl -X POST https://<ref>.<domain>/auth/v1/admin/users -H "apikey: $SECRET" \
  -d '{"email":"test@example.com","email_confirm":true}'
CODE=$(curl -s -X POST https://<ref>.<domain>/auth/v1/admin/generate-link -H "apikey: $SECRET" \
  -d '{"type":"magiclink","email":"test@example.com"}' | jq -r .email_otp)
curl -X POST https://<ref>.<domain>/auth/v1/verify -H "apikey: $PUBLISHABLE" \
  -d "{\"type\":\"magiclink\",\"email\":\"test@example.com\",\"token\":\"$CODE\"}"
```


Deleting a user deletes their sessions and sign-in methods; rows of your
tables that reference them follow your foreign keys.

Monthly active users (anyone who signed in or refreshed a token in the
month) are recorded as usage (`auth_mau`) and shown on the Users tab.

## Storage

Files live in **buckets**. A file's metadata is a row in the project's own
database (`xb_storage.objects`: bucket, path, size, MIME type, checksum,
owner, your metadata), and its bytes are in the region's object store (the
same store as the region's backups; for data-residency projects, the
in-country one). Who may read, upload, overwrite and delete files is decided
by **row-level security on `xb_storage.objects`**, the same policies as
your tables. The project's owner role owns the table, so you write the
policies:

```sql
-- Each user reads and writes only under avatars/<their id>/.
CREATE POLICY own_avatar ON xb_storage.objects FOR ALL TO <db>_user
  USING (bucket = 'avatars' AND xb_storage.folder(path, 1) = xb_auth.uid()::text)
  WITH CHECK (bucket = 'avatars' AND xb_storage.folder(path, 1) = xb_auth.uid()::text);
```

Helpers: `xb_storage.foldername(path)` (the folders, as an array),
`xb_storage.folder(path, n)` (the nth), `xb_storage.filename(path)` and
`xb_storage.extension(path)`. The edge checks each request by running the
matching statement on `xb_storage.objects` as the caller (an insert for
an upload, an update for an overwrite, a select for a read, a delete for a
delete) before it touches the bytes. With row-level security off on
`xb_storage.objects`, the publishable key gets `403 rls_required`; the
secret key bypasses policies.

### Buckets

Buckets are made with the secret key (`POST /storage/v1/bucket`), in
Project → Storage, or with `xenibase storage buckets create`. A bucket has a
name (lowercase, digits, `.`, `_`, `-`; it is in file URLs), whether it is
**public**, a **file size limit**, **allowed MIME types** (`image/*`,
`application/pdf`; empty allows any), and how long browsers may cache its
files. A public bucket's files can be downloaded by anyone with the URL;
uploads to it still need a policy. Making a bucket private purges the CDN's
cache of its files when a CDN is configured (below). A bucket with files
can't be deleted until it is emptied (`POST /storage/v1/bucket/<id>/empty`,
or the dashboard's delete, which asks).

### Endpoints

Supabase's storage clients work against these paths; the spellings they
use (`/object/public/…`, `/object/sign/…`, `/render/image/…`) are accepted
too.

| Request | |
| --- | --- |
| `POST /storage/v1/object/<bucket>/<path>` | Upload up to 50 MB (`x-upsert: true` to overwrite; `PUT` always overwrites) |
| `GET /storage/v1/object/<bucket>/<path>` | Download, as the caller's policies allow; `Range` works |
| `GET /storage/v1/object/info/<bucket>/<path>` | The file's metadata |
| `GET /storage/v1/public/<bucket>/<path>` | A public bucket's file, no key |
| `POST /storage/v1/object/sign/<bucket>/<path>` | A signed download URL (`expires_in` seconds, 1 hour by default, 7 days at most; `transform` for an image size) |
| `POST /storage/v1/object/sign/<bucket>` | Signed URLs for several `paths` |
| `POST /storage/v1/object/upload/sign/<bucket>/<path>` | A signed upload URL (2 hours) for a client without a session |
| `POST /storage/v1/upload/<bucket>/<path>` | Start a large upload (`size`, `mime_type`) |
| `GET`, `DELETE /storage/v1/upload/<id>`; `POST …/<id>/complete` | Its status and the parts still to send; abort; finish |
| `GET /storage/v1/list/<bucket>?prefix=&cursor=&limit=` | Files and folders under a prefix (`POST /storage/v1/object/list/<bucket>` takes the same as JSON) |
| `POST /storage/v1/object/move`, `/copy` | `{"bucket", "from", "to"}`, and `to_bucket` for another bucket |
| `DELETE /storage/v1/object/<bucket>/<path>`; `DELETE /storage/v1/object/<bucket>` | Delete one, or `{"paths": […]}` |
| `GET /storage/v1/render/<bucket>/<path>?width=&height=&resize=&format=&quality=` | An image transform |

Paths are UTF-8 with no empty, `.` or `..` segments and no control
characters, at most 1,024 bytes. A file's type is sniffed from its first
bytes: a declared type that contradicts them (HTML posing as a PNG) is
refused with `400 mime_mismatch`, and a type the bucket doesn't allow with
`415 mime_not_allowed`. Over the bucket's limit or the plan's largest
upload is `413 too_large`; over the organisation's file storage quota is
`413 quota_exceeded`.

### Large uploads

Files over 50 MB, up to the plan's largest upload (5 GB on Pro and Team),
go straight to the object store. Starting one checks the policies and the
quota as an upload would, and answers with presigned URLs for its parts
(16 MiB or more each, at most 10,000):

```json
{"id": "…", "size": 2147483648, "part_size": 16777216, "parts": [{"number": 1, "size": 16777216, "url": "https://…"}, …],
 "expires_at": "…"}
```

`PUT` each part's bytes to its URL; a failed part can be sent again.
`GET /storage/v1/upload/<id>` lists the parts not yet received (with fresh
URLs), so an app can resume after a restart. `POST …/complete` checks the
parts and sizes and writes the file's row as the user who started the
upload. An upload not completed in 24 hours is abandoned and its parts
removed.

### Signed URLs, caching and the CDN

A signed URL carries a token over the project, bucket, path, expiry (and
transform), signed with a key derived for the project, so the edge serves
it without a policy query: the policies were checked when it was made.
Files are served with `ETag` and `Cache-Control` (`public` for public
buckets, `private` otherwise, for the bucket's cache time), and
`Content-Disposition: attachment` with `?download` (or `?download=<name>`). Put a CDN in front of
the edge's hostname to cache public files; with Cloudflare, set
`XENIBASE_CDN_CLOUDFLARE_ZONE_ID` and `XENIBASE_CDN_CLOUDFLARE_TOKEN` (a token
with Cache Purge) on xenibase-server so a bucket made private is purged.

### Image transforms

`/render/…` resizes (`width`, `height` up to 2,500 px; `resize` is
`cover`, `contain` or `fill`), converts (`format`: `webp`, `avif`, `jpeg`,
`png`, or `origin`) and sets `quality` (20–100) for PNG, JPEG, GIF, WebP
and AVIF sources up to 25 MB and 25 megapixels. Each result is kept in the
object store next to the file's version, so the second request for the
same transform is served from that cache (`X-Cache: HIT`) without
rendering; replacing or deleting the file removes them. Transforms are
counted per month against the plan (500 on Personal), as are file
downloads' bytes (5 GB on Personal); once either allowance is used up,
renders answer `429 transform_limit` or downloads `429
storage_egress_limit` until the month turns.

Renders run in separate worker processes (`xenibase-edge render-worker`), one
image at a time each, as many as half the edge's CPUs. A worker has its own
memory limit (`XENIBASE_EDGE_RENDER_MEMORY_MB`, 512 by default); an image
that crashes its decoder or runs it out of memory ends only that worker:
the request gets `500 transform_failed` and the next render starts a new
worker.

### Behind the scenes

- **Versions.** Each upload writes a new object under a new version, and
  the row points at it; the old version's bytes are removed a minute
  later, so a download in flight finishes. A trigger on
  `xb_storage.objects` queues the bytes to remove and keeps the project's
  totals.
- **The sweep** (every 5 minutes, xenibase-server) removes queued bytes and
  abandoned uploads, records storage used (`storage_gb_hours`) and the
  month's downloads and transforms, and sends the edges each project's
  remaining quota.
- **The reconciler** (nightly) removes bytes with no row after 7 days and
  counts rows whose bytes are missing; Project → Storage shows them.
- **Deleting a project** removes its files once its final backup's
  retention ends (30 days).
- **Branches and restores** copy the database, so file rows come along but
  not the bytes: a branch's files show as missing until uploaded again.
  Moving a project to another region doesn't move its files yet.

### The dashboard and the CLI

Project → Storage lists buckets with their size, browses folders, uploads
(up to 50 MB from the browser), downloads, makes signed URLs, deletes files
and changes bucket settings, as the platform (policies don't apply).
Project admins and developers can use it; read-only members can't, since
files may hold your users' data. The CLI does the same:

```sh
xenibase storage buckets create <p> avatars --types 'image/*' --size-limit 5MB
xenibase storage cp <p> ./logo.png ss:///avatars/logo.png
xenibase storage ls <p> ss:///avatars/
xenibase storage cp <p> ss:///avatars/logo.png ./logo.png
xenibase storage sign <p> ss:///avatars/logo.png --expires 24h
xenibase storage rm <p> ss:///avatars/logo.png
```

## Realtime

Clients open one WebSocket, `wss://<ref>.<domain>/realtime/v1/websocket?apikey=<key>`,
and join channels on it. The protocol is the Phoenix channels protocol
Supabase's realtime clients speak (`vsn` 1.0.0 or 2.0.0), so they connect
unchanged:

```js
const client = createClient("https://<ref>.<domain>", "<publishable key>")
client.channel("todos")
  .on("postgres_changes", { event: "*", schema: "public", table: "todos", filter: "list_id=eq.7" },
      (change) => render(change))
  .subscribe()
```

A channel can carry database changes, broadcast and presence at once.
Signed-in clients send their access token in the join (supabase-js does);
a channel may be told a new one with an `access_token` message before
the old expires, and a channel whose token has expired is closed within
10 seconds (at once if a message is due to it or it sends one). A
connection opened with a bearer token takes that token's expiry.

### Database changes

Turn realtime on for a table in Project → Realtime, with
`xenibase realtime enable <p> public.todos`, or from SQL as the owner:

```sql
SELECT xb_realtime.enable('public.todos');   -- and xb_realtime.disable(…)
```

The table needs a primary key, and it must be in an exposed schema.
Turning it on adds a trigger that records each committed insert, update
and delete in `xb_realtime.outbox` in the same transaction, so a
rolled-back change is never delivered. xenibase-edge holds one `LISTEN`
connection per project while it has subscribers (through the region's
session-mode pooler; without one it polls every second), reads the outbox
in transaction order, and delivers each change only to subscribers who may
see the row:

- **Row-level security decides.** For each change the edge reads the row
  as each group of subscribers (the same role and claims; token times and
  session ids aside) in one query per group, and sends a subscriber the
  row as they see it. Anon and signed-in users can subscribe only to
  tables with row-level security on (or listed as public tables); the
  secret key sees everything. Turning row-level security off closes their
  subscriptions to the table at its next change (a `system` message with
  `status` `error`); subscribe again once it is back on.
- **Deletes** carry the primary key only (`old_record`), and reach only
  subscribers the edge sent the row to before: a client that loads rows
  over the data API and then subscribes gets deletes for rows that change
  after it subscribed, so refetch on reconnect.
- **Updates** carry the new row in `record` and the primary key in
  `old_record`.
- **Filters**: `column=eq.value`, `neq`, `lt`, `lte`, `gt`, `gte` and
  `in.(a,b)`, on the row as the subscriber sees it. Deletes aren't
  filtered.
- **Delivery is at most once** to connected clients, in transaction order.
  Nothing is replayed after a disconnect: refetch what you show when you
  reconnect. When the edge can't keep up it tells a channel to refetch
  instead, with a `system` message whose `message` is `resync`: past 200
  changes a second for a project, past 100 distinct groups of subscribers
  on a table, or after its database connection was lost.

### Broadcast and presence

Broadcast sends a message to everyone on a channel (`self: true` to get
your own back, `ack: true` to have the edge confirm it); presence keeps
who is on a channel with each client's state (`track`, `untrack`,
`presence_state`, `presence_diff`). Clients on different xenibase-edge
processes reach each other through `NOTIFY` on the project's database (a
large message in several), and
a process that stops is dropped from presence within about 35 seconds.

A server can broadcast over HTTP with the secret key:

```sh
curl -X POST https://<ref>.<domain>/realtime/v1/api/broadcast -H "apikey: $SECRET_KEY" \
  -H "Content-Type: application/json" -d '{"messages":[{"topic":"room-1","event":"notice","payload":{"text":"hi"}}]}'
```

Broadcasts on topics listed in Project → Realtime → Broadcast history
(`xb_realtime.persisted_topics`) are kept 7 days and read with
`GET /realtime/v1/history/<topic>?after=<id>&limit=`: that is the public
channel's history; add `private=true` for the private channel's, which
needs its read policy (or the secret key). Broadcasts kept before this
release are treated as private.

### Private channels

A channel joined with `private: true` is decided by your policies on
`xb_realtime.channel_access` (owned by the project's owner): SELECT
policies decide who may join and receive, INSERT policies who may send
broadcasts and track presence. A row's `topic` is the channel name (without
`realtime:`) and `extension` is `broadcast` or `presence`:

```sql
CREATE POLICY members_read ON xb_realtime.channel_access FOR SELECT TO <db>_user
  USING (EXISTS (SELECT 1 FROM room_members m WHERE m.room = topic AND m.user_id = xb_auth.uid()));
CREATE POLICY members_send ON xb_realtime.channel_access FOR INSERT TO <db>_user
  WITH CHECK (EXISTS (SELECT 1 FROM room_members m WHERE m.room = topic AND m.user_id = xb_auth.uid()));
```

The edge checks them in a transaction it rolls back when a client joins,
when it sends a new access token, and again every minute: a user your
policies no longer allow is closed out of the channel.
A private channel and a public one of the same name don't hear each other.

### Limits and use

| | Personal | Pro | Team |
| --- | --- | --- | --- |
| Concurrent connections (per edge process) | 100 | 1,000 | 5,000 |
| Messages a month | 1 million, then refused (open connections are closed) | metered | metered |

Clients send a heartbeat every 25 seconds; a connection silent for 2
minutes is closed, as is one too slow to take its messages (it reconnects
and refetches). Open connections are closed (code 1008) when the key they
were opened with is revoked or rotated away, when the organisation is
suspended, and when backend services are turned off. A connection joins at most 100 channels, sends at most 600
broadcasts and presence updates a minute, and a broadcast or presence
message is at most 32 KB (other frames 64 KB). A channel may have at most 20
`postgres_changes` subscriptions and a connection 100, and a client that
has 4 MB waiting for it is disconnected. A project's clients may
send at most 1 MB of broadcasts and presence a second through each
xenibase-edge process, and a topic kept as history keeps at most 256 KB a
minute per process; past these a broadcast is refused with a
`rate_limited` error reply (`429` over HTTP, with how many were sent). Messages to and from clients (`realtime_messages`: broadcasts,
presence and database changes; heartbeats, joins and replies are free) and
connection time (`realtime_connection_minutes`) are metered.

## Settings

- **Allowed origins**: the pages that may call the API. Empty allows any
  origin, which is fine for the publishable key; list your app's origins
  once you go live. For an allowed origin the edge answers a browser's
  preflight by allowing the request headers the browser says it will send, so a
  client that adds a header (`x-client-info`, which every Xenibase and Supabase
  client sends) is never refused for it.
- **Statement timeout**: each request's limit, 8 seconds by default and 15
  at most.
- **Rate limits**: requests per minute per IP address (600) and per key
  (12,000). Over the limit, the edge answers `429` with `Retry-After`.
- **Exposed schemas**: the schemas the data API serves (`public`). Exposing
  another grants the request roles use of what the owner makes there.
  Platform schemas (`xb_*`, `pg_*`, `xenibase`) can't be exposed.
- **Public tables**: tables and views the publishable key may read without
  row-level security. Anyone with your app can read them.
- **Maximum query cost**: the cost guard's limit (above).
- **Read from replicas by default** (dedicated projects with read
  replicas): publishable-key data API GETs go to the replicas without a
  `Read-Replica` header. Any GET can ask with `Read-Replica: allowed`, or
  stay on the primary with `Read-Replica: primary`. Replicas trail writes
  by up to 10 seconds; see docs/read-replicas.md.

A paused Free project answers `503 project_resuming` with `Retry-After` and
is woken, as a database connection would wake it, when the request carries
one of its live API keys. Without one (a public file, an OAuth callback, or
anyone who only knows the hostname) it answers `503 project_paused` and
stays asleep. A suspended
organisation's projects answer `403 project_suspended`.

## Usage and logs

The edge counts each request that passed the key check (`api_requests`)
and the bytes it returned (`api_egress_gb`), per project and hour, and
sends them to xenibase-server every few seconds with the request log. A
report is recorded once even if it is retried. The request log (method,
path, status, latency, role, user, key, IP) is on the API page and at
`GET /api/v1/projects/{id}/services/logs`, kept 7 days.

When the organisation reaches its spend cap ([billing](billing.md)),
the project slows down instead of stopping: data and storage requests
get a quarter of the usual rate limits (`429 spend_cap_rate_limited`),
new image transforms answer `429 spend_cap_reached` (cached ones still
serve), and new realtime connections are refused with `429
spend_cap_reached` while open ones stay. Auth endpoints are unaffected.

## Plan limits

Each quota plan ([quotas](operations.md#quotas-storage-locks-and-usage))
can set monthly hard limits and ceilings on a project's own settings. A key the plan leaves out has no
limit; an organisation's overrides (Admin → Organisations) win over the
plan.

| Limit | Key | Personal | Pro | Team |
| --- | --- | --- | --- | --- |
| Data API requests a month | `api_requests_per_month` | 500,000 | — | — |
| Monthly active users | `auth_mau_per_month` | 10,000 | — | — |
| Statement timeout ceiling (ms) | `api_timeout_ms` | 5,000 | 8,000 | 15,000 |
| Requests per minute per IP, ceiling | `api_rate_per_ip_per_min` | 300 | 600 | 1,200 |
| Requests per minute per key, ceiling | `api_rate_per_key_per_min` | 3,000 | 12,000 | 30,000 |
| SMS codes a day (platform sender) | `sms_codes_per_day` | — | 1,000 | 5,000 |

- **Monthly requests.** Once an organisation's data API requests this
  month reach the limit, the data API, storage and realtime answer
  `429 plan_limit_reached` with a `Retry-After` up to the start of next
  month. Auth and `GET /data/v1/health` keep working, so users can still
  sign in and an app can say why. Paid plans have no request limit: they
  are billed past what the plan includes.
- **Monthly active users.** At the limit, users who already signed in
  this month keep signing in and refreshing their sessions; a new user
  gets `429 mau_limit_reached`. The edge gets a compact filter of the
  users counted this month (about 12 KB for 10,000 users), so it can
  answer without asking xenibase-server; about one new user in a hundred
  can still slip through until the next count.
- **Ceilings.** A project's timeout and rate limits are capped at the
  plan's: asking for 15 s on a 5 s plan gets 5 s. The API page (Access
  and limits) and `xenibase services status` show what is in effect.
- **SMS codes.** Codes sent with Xenibase's own sender are capped per
  project and day at the lower of the project's cap and the plan's; a
  project's own provider has only its own cap. Free plans send codes only
  when the operator allows it (`XENIBASE_PHONE_AUTH_FREE`).

xenibase-server works the limits out every five minutes, so a limit
reached or lifted (an upgrade, an override) reaches the edges within
about five minutes. Owners and admins get an email at 80% and at 100% of
each monthly limit, once a month each.

On an install upgrading from an earlier version, the limits apply from
the first full month after the upgrade (the `plan_limits_from` setting);
notices before then say when they start.

## Running xenibase-edge

Run one xenibase-edge per region, on the region's nodes. It keeps no state.

| Setting | |
| --- | --- |
| `XENIBASE_API_DOMAIN` (server) | The API domain: projects are at `<ref>.<domain>`. |
| `XENIBASE_EDGE_SECRET` (server) | At least 32 characters. Without it xenibase-server serves no edge. It is the platform-wide secret, and serves only the regions without their own API domain. |
| `XENIBASE_EDGE_SECRET` (edges) | The edge's own credential (see below), or for an edge of the platform's API domain, the server's value. |
| `XENIBASE_EDGE_SECRET_ALL_REGIONS` (server) | Deprecated: `true` lets the platform-wide secret serve every region again, as before edges had their own credentials. |
| `XENIBASE_EDGE_CONTROL_URL` | xenibase-server's URL as the edge reaches it. |
| `XENIBASE_EDGE_DOMAIN` | The API domain it serves: `XENIBASE_API_DOMAIN`, or for an edge in a region with its own API domain, that domain ([Regions](regions.md#a-regions-api-domain)). |
| `XENIBASE_EDGE_REGION` | The region it serves (empty for all it may see). An edge for a region's own API domain sets it to that region. |

**Edge credentials.** The server decides which regions an edge sees from
how it signs, not from `XENIBASE_EDGE_REGION`. An edge in a region with its
own API domain needs a credential of its own, bound to that region:

```sh
docker compose exec xenibase-server xenibase-server -issue-edge-credential edge-lagos-1 -edge-region ng-lagos
```

prints `xbedge_<id>_<secret>`; put it in the edge's `XENIBASE_EDGE_SECRET`.
Running it again for the same name prints the same credential;
`-revoke-edge-credential edge-lagos-1` revokes it. An edge with a credential
gets only its region's projects (asking for another region is refused), and
can wake, send auth messages and hooks, raise storage events and report
usage only for them. A compromised regional edge therefore exposes one
region. Edge nodes Xenibase creates get their own credential in their
cloud-init. The platform-wide secret still serves the edges of the
platform's API domain (the bundle's own edge among them), for the regions
whose projects use that domain.
| `XENIBASE_EDGE_TLS_CERT`, `XENIBASE_EDGE_TLS_KEY` | A wildcard certificate for `*.<domain>`. |
| `XENIBASE_EDGE_LISTEN` | Default `:8443`. |
| `XENIBASE_EDGE_POOLER_ADDR` | Optional: the transaction pooler as the edge reaches it. |
| `XENIBASE_EDGE_SESSION_ADDR` | Optional: the session-mode pooler as the edge reaches it (realtime's `LISTEN` connections). |
| `XENIBASE_EDGE_POOLER_SSLMODE` | TLS to the poolers: `require` (default: encrypted, certificate not checked), `verify-ca` or `verify-full` (checked; use it when the edge crosses a network you don't control, such as a regional edge reaching the home region's poolers). `prefer` and below are refused: the service logins' sessions would fall back to plain text. |
| `XENIBASE_EDGE_POOLER_ROOT_CERT` | Optional: a PEM file of the CA that signed the poolers' certificate, for `verify-ca`/`verify-full` with a self-signed or private certificate. A certificate from a public CA (`XENIBASE_POOLER_TLS=acme`) verifies against the system's roots without it. |
| `XENIBASE_EDGE_TRUSTED_PROXIES` | Optional: CIDRs allowed to set `X-Forwarded-For`. |
| `XENIBASE_EDGE_CACHE_MB` | Optional: the anonymous-read cache's size per edge process (default 64). |
| `XENIBASE_EDGE_RENDER_MEMORY_MB` | Optional: each image render worker's memory limit (default 512, at least 64). |
| `XENIBASE_EDGE_RENDER_IN_PROCESS` | Optional: set to render images inside the edge process instead of in workers (not recommended; a bad image can then take the edge down). |

`deploy/edge/Dockerfile` builds the image, and `deploy/edge/edge.env.example`
lists the settings.

### With the compose install

The bundle can run the edge on the same server, behind its Caddy, with the
wildcard certificate that Caddy gets and renews with a DNS challenge. The
full method, with every step, what the script changes, how to operate it and
the routes that were tried, is in
[Enabling backend services](enabling-backend-services.md). In short:

```sh
cd deploy/compose
# DNS: *.api A record; credentials for the challenge in .env (by hand)
./enable-services.sh api.example.com acmedns     # or cloudflare, or desec
```

The edge is limited to 1 GB of memory (`XENIBASE_EDGE_MEMORY`); image
transforms run in workers of up to 512 MB each. To turn it off again, remove
`COMPOSE_PROFILES=services` from `.env`, delete `caddy.d/api.caddy`, and run
`docker compose up -d --remove-orphans`.

- **DNS:** a wildcard record `*.<domain>` pointing at the region's edge.
- **TLS:** get the wildcard certificate with a DNS-01 client for your DNS
  provider (certbot, lego, acme.sh) and point the two settings at its
  files. The edge re-reads them when they change. Behind a TLS terminator,
  leave them empty and the edge serves plain HTTP.

The edge follows xenibase-server's configuration feed. A change, such as a
key revoked, an origin added, a project paused or an organisation
suspended, reaches it within a second or two, and it re-reads everything
every minute in case a change was missed. While xenibase-server is down, the
edge keeps serving from what it has and holds its reports, retrying them
later. `GET /healthz` on any host that isn't a project answers once the
first configuration has loaded.

## Platform SMS and WhatsApp (operators)

xenibase-server sends projects' codes when they don't bring their own
provider. Set:

| Variable | |
| --- | --- |
| `XENIBASE_TERMII_API_KEY` (or `_FILE`), `XENIBASE_TERMII_SENDER_ID` | Termii's API key and an approved sender ID. Codes go by Termii's `dnd` channel. `XENIBASE_TERMII_URL` is the account's API base if it isn't `https://api.ng.termii.com`. |
| `XENIBASE_AFRICASTALKING_USERNAME`, `XENIBASE_AFRICASTALKING_API_KEY` (or `_FILE`), `XENIBASE_AFRICASTALKING_FROM`, `XENIBASE_AFRICASTALKING_URL` | Africa's Talking, the fallback for SMS. With both set, a code goes by Termii and, if Termii fails, by Africa's Talking at once; a provider that failed is tried last for a minute, so an outage costs one timeout rather than one per code. Each failure is logged (`platform SMS provider failed`) and the provider that sent each code is recorded in `message_sends`. Either alone also works. |
| `XENIBASE_WHATSAPP_OTP_TEMPLATE`, `XENIBASE_WHATSAPP_OTP_LANGUAGE` | An approved **authentication** template (with a copy-code button) on the WhatsApp number support uses (`XENIBASE_WHATSAPP_PHONE_NUMBER_ID`, `XENIBASE_WHATSAPP_ACCESS_TOKEN`); the language defaults to `en`. |
| `XENIBASE_SMS_PRICE_MINOR`, `XENIBASE_WHATSAPP_PRICE_MINOR`, `XENIBASE_MESSAGE_CURRENCY` | What a message costs the project, in minor units (default NGN 4.50 and NGN 15.00), shown as spend; usage is metered as `messages_sms` and `messages_whatsapp`. |
| `XENIBASE_PHONE_AUTH_FREE` | `true` lets Free projects use the platform's providers (by default they bring their own). |

Without them, phone sign-in works only for projects with their own
provider.

## Status and limits

Backend services are ready for general availability (V4-M37): the data
API, auth, storage and realtime are billed as the plan's
[prices](billing.md) say, spend caps slow them rather than stop them, and
they have had a security review of the edge with fuzz tests of its
isolation ([security review](security-review.md#v4-review-of-the-edge-m37)),
load tests ([load test](load-test.md#v4-backend-services-load-test)) and
failure injection (an edge killed mid-upload, an SMS provider outage, a
realtime process lost). What to do when something breaks is in the
[runbook](backend-runbook.md). An operator announces GA once every
[GA gate](backend-runbook.md#ga-gates) has a dated result, the external
penetration test of the edge ([scope](pentest-scope.md)) among them with no
open critical or high finding.

Not built: realtime over logical decoding for high-volume tables, and
subscriptions to every table at once; copying files into a branch,
moving files with a project that changes region, and malware scanning of
uploads; auth's leaked-password check and bounce handling for auth
emails.
