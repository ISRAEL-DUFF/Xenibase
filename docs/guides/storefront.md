# Building on Xenibase: a team guide

This guide is for developers who have never used Xenibase's backend services
and are about to build on them. It takes a **domain storefront** (customers
sign up, see a price list, order domains, watch the order's progress and
download a receipt) from nothing to a working, safe backend, and explains each
idea the first time it is needed.

You will use four services, all from the same project and the same keys:

| Service | What it does here |
| --- | --- |
| **Data API** | Reads and writes your tables over HTTPS, as the signed-in customer, with the database deciding who may see what. |
| **Auth** | Sign-up, sign-in (email code, password, phone or WhatsApp code), sessions. |
| **Storage** | Files such as receipts, with the same kind of access rules. |
| **Realtime** | Pushes changes (an order's status) to the customer's open page. |

Xenibase also sends a signed **webhook** to your server when a row changes. That
is how the storefront's server finds out that an order was paid and has to be
fulfilled. The reference for everything below is
[Backend services](../backend-services.md); this page is the route through it.

Contents: [1. The model](#1-the-model-in-ten-minutes) ·
[2. Set up the project](#2-set-up-the-project) ·
[3. Install the client](#3-install-the-client) ·
[4. Design the data](#4-design-the-data-and-protect-it) ·
[5. Sign-in](#5-sign-in) · [6. Reading and writing](#6-reading-and-writing) ·
[7. Orders: the server side](#7-orders-the-server-side-of-the-storefront) ·
[8. Files](#8-files-receipts) · [9. Live updates](#9-live-updates) ·
[10. Working day to day](#10-working-day-to-day) ·
[11. When something is wrong](#11-when-something-is-wrong) ·
[12. Before launch](#12-before-launch-checklist) ·
[13. Giving feedback](#13-giving-us-feedback) ·
[Quick reference](#quick-reference)

> **If you were sent this page as a single file,** its links to other pages don't
> work. They're all in the repository's `docs` folder:
> <https://github.com/ISRAEL-DUFF/Xenibase/tree/main/docs>. The ones this guide
> uses are `backend-services.md`, `errors.md`, `webhooks.md`,
> `supabase-client-mapping.md`, `cli.md`, `sdk/javascript.md` and
> `guides/nextjs.md`.

## 1. The model in ten minutes

**A project is one database plus its API.** Each project has its own Postgres
database and, once backend services are on, its own address:
`https://<ref>.api.xenibase.ng`. `<ref>` is eight letters and digits
(`k7f3m2q9`). Everything the app calls lives under that address:
`/data/v1/…` (tables), `/auth/v1/…` (sign-in), `/storage/v1/…` (files) and
`/realtime/v1/…` (live updates).

**There are two keys, and they are not interchangeable.**

| Key | Looks like | Goes in | Acts as |
| --- | --- | --- | --- |
| **Publishable** | `xb_pub_…` | The browser and mobile apps. It is meant to be public. | A visitor (`anon`), or the signed-in customer when their token is sent with it. |
| **Secret** | `xb_sec_…` | **Your server only.** Never in a page, an app, a repository or a chat. | The service role: it **skips every access rule**. |

Send a key in the `apikey` header. A signed-in customer's access token goes in
`Authorization: Bearer …` next to it.

**The database decides who may see what, not your code.** Requests run inside
Postgres as one of three roles (`<db>_anon`, `<db>_user`, `<db>_service`), and
**row-level security** (RLS) policies on each table say which rows each role
may read or change. This is the single most important idea: if a table has no
policy for a role, that role gets nothing. A table with RLS switched off isn't
readable with the publishable key at all (`403 rls_required`). You write the
rules once, in SQL, and every client (web, mobile, scripts) obeys them.

`<db>` is your project's database name (it starts with `p_`); Project → API
shows the three role names. In the SQL below, replace `<db>` with it.

**What the customer's token is.** Signing in returns an access token (a signed
JWT, an hour by default) and a refresh token. Inside a policy,
`xb_auth.uid()` is the signed-in customer's id. Customers live in the
project's own database (`xb_auth.users`), so your tables can point at them with
an ordinary foreign key.

## 2. Set up the project

You need an account in the Xenibase organisation that owns the project. Ask an
owner to invite you (Organisation → Members). In the project, **developers**
read and write data, change the schema, and manage webhooks and jobs;
**admins** also manage settings and keys; **read-only** members see the project
but can't use storage (files may hold customers' data). Organisation owners and
admins are admins of every project.

1. **Create a project** (Projects → New project). For a storefront use a
   paid plan: Free projects pause after a week without connections, and a
   paused project answers `503 project_resuming` until it wakes.
2. **Turn on backend services:** Project → Settings → API → **Enable backend
   services**. Xenibase shows the project's address and its keys **once**.
   Save the secret key in the team's password manager straight away; it can't
   be shown again (you can make a second one and revoke the first).
3. **Check it answers**, from your computer:

   ```sh
   curl -H "apikey: xb_pub_…" https://<ref>.api.xenibase.ng/data/v1/health
   # {"status":"ok","project":"<ref>","role":"anon","region":"…"}
   ```

4. **Set the project's settings** (Settings → API and Authentication). Do these now,
   not at launch:
   - *Site URL* and *redirect URLs* (Authentication → *Your app*): the
     addresses sign-in links may send people back to, including
     `http://localhost:3000` for development. Anything else is refused
     (`redirect_not_allowed`).
   - *Allowed origins* (Settings → API): empty allows any page to call the API,
     which is fine with the publishable key while developing. Before launch,
     list only your storefront's addresses.
5. **Keep keys out of the code.** Environment variables, never committed:

   ```sh
   # .env.local (browser-safe: the publishable key only)
   NEXT_PUBLIC_XENIBASE_URL=https://<ref>.api.xenibase.ng
   NEXT_PUBLIC_XENIBASE_KEY=xb_pub_…

   # server-only (not NEXT_PUBLIC_…, never sent to the browser)
   XENIBASE_SECRET_KEY=xb_sec_…
   XENIBASE_WEBHOOK_SECRET=…
   ```

   A secret key sent from a web page (a request with an `Origin` header) is
   refused, which protects you from the easiest mistake: if you ever see
   `secret_key_in_browser`, the secret key has reached client code. Treat that
   key as leaked and replace it.

## 3. Install the client

The JavaScript client is [`@xenibase/client`](https://www.npmjs.com/package/@xenibase/client)
(works in browsers, Node 18+ and React Native, no dependencies, Apache-2.0; ES
modules and CommonJS, so it works from a script run with `tsx` or `node` in a
package with or without `"type": "module"`):

```sh
npm install @xenibase/client
```

Pin the version in `package.json` while the client is `0.x`, and read the
changelog before upgrading: a `0.x` minor version may change an API.

If you already use `@supabase/supabase-js`, it also works against a Xenibase
project, because the API speaks the same wire protocol for data, auth, storage
and realtime paths. See [the Supabase client mapping](../supabase-client-mapping.md)
for what maps and what differs. Prefer `@xenibase/client` for new code: it
matches this guide.

Then generate types from the live schema, so rows and inserts are checked at
compile time (re-run it after every schema change):

```sh
xenibase gen types --lang ts --project <project-name> > lib/database.types.ts
```

The `xenibase` command line is installed from a release (see [the CLI](../cli.md))
and signs in with `xenibase login --server https://app.xenibase.ng`.

Create the client once:

```ts
// lib/xenibase.ts
import { createClient } from "@xenibase/client";
import type { Database } from "./database.types";

export const xb = createClient<Database>(
  process.env.NEXT_PUBLIC_XENIBASE_URL!,
  process.env.NEXT_PUBLIC_XENIBASE_KEY!,
);
```

**Calls don't throw for API errors.** Each resolves to `{ data, error }`; check
`error` (it has `status`, `code`, `message` and `requestId`):

```ts
const { data, error } = await xb.data.from("tld_prices").select();
if (error) console.error(error.code, error.requestId);
```

For the server-side half of a Next.js app (reading as the signed-in customer in
server components) follow [the Next.js guide](nextjs.md); it shows the cookie
hand-off between browser and server.

## 4. Design the data, and protect it

Create the schema in the SQL editor (Project → SQL) or with `psql` through the
session-mode connection string, as the project's owner. Keep every statement in
a file in your repository (`db/001_storefront.sql`) so the schema is reviewed
and repeatable. This is the storefront's core. It has been run against a real
Postgres and checked: the checks are described after the code.

**Money** is stored in **kobo** (`bigint`, integer minor units), never as
floating point. Prices come from the database, not from the browser.

```sql
-- Who the customers are: one profile per signed-in user.
CREATE TABLE profiles (
  user_id    uuid PRIMARY KEY DEFAULT xb_auth.uid() REFERENCES xb_auth.users (id) ON DELETE CASCADE,
  full_name  text NOT NULL,
  phone      text,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE profiles ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_profile ON profiles FOR ALL TO "<db>_user"
  USING (user_id = xb_auth.uid()) WITH CHECK (user_id = xb_auth.uid());

-- The price list: anyone may read it, only your server may change it.
CREATE TABLE tld_prices (
  tld            text PRIMARY KEY,                       -- '.ng', '.com.ng', '.africa'
  register_minor bigint NOT NULL CHECK (register_minor >= 0),
  renew_minor    bigint NOT NULL CHECK (renew_minor >= 0),
  transfer_minor bigint NOT NULL CHECK (transfer_minor >= 0),
  active         boolean NOT NULL DEFAULT true,
  updated_at     timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE tld_prices ENABLE ROW LEVEL SECURITY;
CREATE POLICY anyone_reads_prices ON tld_prices FOR SELECT TO "<db>_anon", "<db>_user" USING (active);
-- No INSERT/UPDATE/DELETE policy: only the secret key (your server) can change prices.

-- Orders and their lines. Customers can read their own; they cannot write them.
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'fulfilling', 'completed', 'failed', 'cancelled');
CREATE TABLE orders (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES xb_auth.users (id),
  status      order_status NOT NULL DEFAULT 'pending',
  total_minor bigint NOT NULL DEFAULT 0 CHECK (total_minor >= 0),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE order_items (
  id          bigserial PRIMARY KEY,
  order_id    uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
  name        text NOT NULL,                              -- 'acme'
  tld         text NOT NULL REFERENCES tld_prices (tld),  -- '.ng'
  action      text NOT NULL CHECK (action IN ('register', 'renew', 'transfer')),
  years       int  NOT NULL DEFAULT 1 CHECK (years BETWEEN 1 AND 10),
  price_minor bigint NOT NULL CHECK (price_minor >= 0),
  result      jsonb                                       -- what fulfilment returned
);
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE order_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_orders ON orders FOR SELECT TO "<db>_user" USING (user_id = xb_auth.uid());
CREATE POLICY own_order_items ON order_items FOR SELECT TO "<db>_user"
  USING (EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.user_id = xb_auth.uid()));
```

**Customers place orders through one function, so they can't set their own
prices.** The browser never sends a price. It sends *what* it wants, and the
function looks the price up in `tld_prices`:

```sql
CREATE FUNCTION place_order(items jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
  oid uuid;
  it  jsonb;
  p   bigint;
  n   int;
BEGIN
  IF xb_auth.uid() IS NULL THEN
    RAISE EXCEPTION 'sign in first' USING ERRCODE = '28000';
  END IF;
  IF jsonb_typeof(items) <> 'array' OR jsonb_array_length(items) NOT BETWEEN 1 AND 20 THEN
    RAISE EXCEPTION 'an order has 1 to 20 items' USING ERRCODE = '22023';
  END IF;
  INSERT INTO orders (user_id) VALUES (xb_auth.uid()) RETURNING id INTO oid;
  FOR it IN SELECT * FROM jsonb_array_elements(items) LOOP
    n := coalesce((it ->> 'years')::int, 1);
    SELECT CASE it ->> 'action'
             WHEN 'register' THEN register_minor
             WHEN 'renew'    THEN renew_minor
             WHEN 'transfer' THEN transfer_minor
           END * n
      INTO p FROM tld_prices WHERE tld = it ->> 'tld' AND active;
    IF p IS NULL THEN
      RAISE EXCEPTION 'unknown tld or action: %', it USING ERRCODE = '22023';
    END IF;
    INSERT INTO order_items (order_id, name, tld, action, years, price_minor)
    VALUES (oid, lower(it ->> 'name'), it ->> 'tld', it ->> 'action', n, p);
  END LOOP;
  UPDATE orders SET total_minor = (SELECT sum(price_minor) FROM order_items WHERE order_id = oid)
   WHERE id = oid;
  RETURN oid;
END $$;
REVOKE ALL ON FUNCTION place_order(jsonb) FROM PUBLIC, "<db>_anon";
GRANT EXECUTE ON FUNCTION place_order(jsonb) TO "<db>_user";
```

`SECURITY DEFINER` means the function runs as its owner and **skips RLS**, which
is exactly why it is used here, and exactly why it must stay small, validate
everything and `SET search_path`. Xenibase's security advisor lists these
functions; keep the list short and know why each is on it.

**What was checked** (in a throwaway Postgres with stand-ins for Xenibase's
roles): an anonymous visitor sees only the *active* prices; a signed-in customer
places an order and the total is computed from the price list (two years of
`.ng` plus a `.africa` registration added up correctly); another customer
sees none of it; a customer cannot insert an order directly; an inactive TLD and
an unknown action are refused; and an anonymous caller cannot place an order.
Re-run similar checks against your own project whenever a policy changes.

Three habits that keep this safe:

- **Every table gets RLS and a policy before it holds real data.** Run
  Project → Settings → API → **Security advisor** after each schema change: it
  flags tables with RLS off, RLS on with no policies, and policies open to
  everyone.
- **Views** must be created `WITH (security_invoker = true)`, or they bypass the
  policies of the tables under them.
- **Hide columns**, not only rows, when needed: take the table-level grant
  away and grant the columns that stay readable,
  `REVOKE SELECT ON profiles FROM "<db>_anon"; GRANT SELECT (user_id, full_name, created_at) ON profiles TO "<db>_anon";`
  (a column-level `REVOKE` alone does nothing while the table-level grant
  stands).

**One migration file for every project.** The role names contain the project's
database name, which differs per project. Derive them in the migration instead of
editing the file by hand each time:

```sql
DO $$
DECLARE
  u text := current_database() || '_user';
  a text := current_database() || '_anon';
BEGIN
  EXECUTE format('CREATE POLICY own_orders ON public.orders FOR SELECT TO %I USING (user_id = xb_auth.uid())', u);
  EXECUTE format('CREATE POLICY anyone_reads_prices ON public.tld_prices FOR SELECT TO %I, %I USING (active)', u, a);
END $$;
```

This is the recommended form for anything you keep in the repository. (The
statements above use `"<db>_user"` only to keep them readable.) It was checked in
a real Postgres: the policies come out `TO` the `<database>_user` and
`<database>_anon` roles.

Check what the API now exposes in **Project → Settings → API → API docs**: each
table, its policies in plain terms, and curl and TypeScript examples built from
the real schema. **Try it** in the same page sends a request as anon, as a
specific user or as the service role, without handing out a key.

## 5. Sign-in

### Choose the sign-in methods

Project → Authentication → *Sign-in methods* controls them. For Nigerian customers a **WhatsApp or
SMS code** is often the lowest-friction option; email codes and passwords also
work, and you can allow several.

- **Email:** the customer gets a link and a six-digit code (valid for ten
  minutes, single use). *With "Confirm email addresses" on (the default) a new
  password account can't sign in until it is confirmed.*
- **Phone:** numbers may be written `0803 123 4567` or `+2348031234567`; they
  are stored in international form. Codes go out through Xenibase's own sender
  by default; limits stop abuse (Nigeria only by default, one code a minute per
  number, 200 codes a day per project). You can bring your own provider.

### Make email arrive from your name

The platform's built-in email works for development, at **30 emails an hour
per project**. For a real storefront, set your own SMTP server (Authentication
→ Emails) so customers get mail from your domain with no hourly cap, and use
**Send test** before saving. The sign-up, sign-in and reset emails are laid out
cleanly under your project's name; edit the text under Authentication → Emails
(plain text with `{{.Code}}`, `{{.Link}}`, `{{.Email}}`, `{{.SiteURL}}`).

### The calls

```ts
// Sign up with email and password. The session is null until the address is confirmed.
await xb.auth.signUp({ email, password, data: { name: "Ada Obi" } });

// Or a code by email or WhatsApp, then verify it.
await xb.auth.signInWithOtp({ email });                                  // or { phone: "08031234567", channel: "whatsapp" }
await xb.auth.verifyOtp({ type: "email", email, token: "123456" });      // or { type: "whatsapp", phone: "+2348031234567", token }

await xb.auth.signInWithPassword({ email, password });
await xb.auth.signOut();

xb.auth.onAuthStateChange((event, session) => { /* SIGNED_IN, TOKEN_REFRESHED, SIGNED_OUT */ });
```

Tokens refresh by themselves a minute before they expire. **Only one refresh
may run at a time**: a refresh token works once, and presenting it twice ends
the whole session (`refresh_token_reused`). Don't call the refresh endpoint
yourself in several places; let the client do it.

### Create the profile when a customer first signs in

The easiest way is to insert the profile row from the client after sign-in; the
policy lets a customer insert only their own:

```ts
const { data: { user } } = await xb.auth.getUser();
await xb.data.from("profiles").upsert({ user_id: user!.id, full_name: "Ada Obi" }, { onConflict: ["user_id"] });
```

To run code at sign-up instead (to refuse certain emails, say), use a
*before sign-up* hook: see Authentication → Hooks and
[Hooks](../backend-services.md#hooks).

### Testing sign-in without an inbox

In automated tests or a seed script, don't wait for an email. Create the user
with your server's secret key, ask the admin API for a code, and trade it for a
session. The endpoints, what each needs, and a worked example are in
[the admin API](../backend-services.md#the-admin-api). Two things trip people up:
`generate-link` does **not** create the user for `magiclink` or `recovery` (create
one first with `email_confirm: true`), and a `redirect_to` has to be the site URL
or one of the project's redirect URLs.

### Administrators

Staff who manage prices or orders are not customers. Give them their own way in
(a separate project role, or just your server with the secret key behind your
own admin login); require **MFA** for them (Authentication → MFA).

## 6. Reading and writing

With the generated types and RLS in place, the client is how pages talk to data.

```ts
// The price list (anonymous visitors can read it).
const { data: prices } = await xb.data.from("tld_prices").select().order("tld");

// The signed-in customer's own orders, newest first, with their lines.
const { data: orders } = await xb.data
  .from("orders")
  .select("id, status, total_minor, created_at, order_items(name, tld, action, years, price_minor)")
  .order("created_at", "desc")
  .limit(20);

// Place an order. The browser sends WHAT, never a price.
const { data: orderId, error } = await xb.data.rpc("place_order", {
  items: [{ name: "acme", tld: ".ng", action: "register", years: 1 }],
});

// Update own profile.
await xb.data.from("profiles").update({ phone: "08031234567" }).eq("user_id", user.id);
```

**Filters:** `eq`, `neq`, `lt`, `lte`, `gt`, `gte`, `in`, `like`, `ilike`, `is`,
`contains`, `search` (full text); combine with `.or(…)` and `.not(…)`. **Pages:**
`limit` (100 by default, 1,000 at most) plus `nextCursor` for any depth.
**Writes need a filter or a key**: an `update` or `delete` with neither is
refused (`filter_required`), which stops a one-line mistake emptying a table.

**The same thing without the client**, so you can test from a terminal:

```sh
curl -G https://<ref>.api.xenibase.ng/data/v1/tld_prices \
  -H "apikey: xb_pub_…" --data-urlencode "order=tld:asc"

curl -X POST https://<ref>.api.xenibase.ng/data/v1/rpc/place_order \
  -H "apikey: xb_pub_…" -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"items":[{"name":"acme","tld":".ng","action":"register","years":1}]}'
```

**Fewer round trips.** Each call is a round trip to the project's region, and from
far away (Lagos to a European project is 300 to 700 ms a call) four calls in a row
is two seconds. Ask for what a page needs in as few calls as possible:

- **Embed related rows in one read** instead of one read per table:
  `.select("id, status, order_items(name, tld, price_minor)")` returns orders with
  their lines in one call (up to three levels deep).
- **Run independent reads together:** `await Promise.all([prices, orders, profile])`,
  so the waits overlap instead of adding up.
- **For a page that needs several different things, write a database function**
  that returns them as one JSON value, mark it `STABLE`, and call it once with
  `xb.data.rpc("storefront_home", …)`. A `STABLE` function called with `GET` can
  also be cached at the edge for anonymous visitors.
- **Cache what everyone reads the same way** (the price list, below).
- **Writes** that belong together go in one `data.batch` (up to 50, all or
  nothing).

**Speed up the price list.** Every visitor reads the same price list, so serve
it from the edge's cache: Settings → API → **Cache anonymous reads**, set
`public.tld_prices` to 60 seconds. Only *anonymous* reads are cached; a write
through the data API clears the table's entries at once, and a change made any
other way shows within the 60 seconds.

**Limits to know:** each request has an 8-second statement timeout, at most 30
filters and 3 levels of related rows, and a 10 MB result; a query whose planned
cost is far too high is refused with `query_too_expensive` (add an index or a
filter). Rate limits are 600 requests a minute per IP and 12,000 per key by
default.

## 7. Orders: the server side of the storefront

Some things must never happen in the browser: marking an order paid, calling the
domain registrar, changing prices. They run on **your server** (a Next.js route
handler, a small service) using the **secret key**. Xenibase has no
"serverless functions" of its own yet, so this is your server's job.

The flow, end to end:

```text
customer          storefront browser           Xenibase                    your server
   │ order ──────────▶ place_order() ───────▶ orders row (pending)
   │ pays (your payment provider) ─────────────────────────────────────────▶ confirms payment
   │                                          ◀──── PATCH orders status=paid ┘  (secret key)
   │                                          UPDATE event ──── signed webhook ─▶ fulfil
   │                                          ◀──── PATCH status=completed ──────┘
   │ ◀─────── live update ─────────────────── (realtime pushes the new status)
```

### a. Confirm payment (server)

Your payment confirmation handler writes the status with the secret key, using
an **Idempotency-Key** so a retry can't run it twice:

```ts
// app/api/payments/confirm/route.ts (after you have verified the payment yourself)
const url = process.env.NEXT_PUBLIC_XENIBASE_URL!;

export async function markPaid(orderId: string, paymentRef: string) {
  const res = await fetch(`${url}/data/v1/orders/${orderId}`, {
    method: "PATCH",
    headers: {
      apikey: process.env.XENIBASE_SECRET_KEY!,
      "Content-Type": "application/json",
      "Idempotency-Key": `paid-${paymentRef}`,
    },
    body: JSON.stringify({ status: "paid" }),
  });
  if (!res.ok) throw new Error(`mark paid: ${res.status} ${await res.text()}`);
}
```

The Idempotency-Key makes a repeat within 24 hours return the first answer and
write nothing (`Idempotent-Replayed: true`). Use a key that identifies the
*real-world event* (the payment's reference), not a random value per attempt.

### b. Tell your server when an order is paid (webhook)

In the app: **Database → Webhooks → New**:

| Field | Value |
| --- | --- |
| Tables | `orders` |
| Events | `UPDATE` |
| Columns | `status` (so it fires only when the status changes) |
| URL | `https://storefront.example.com/api/xenibase/webhook` (public `https://`) |

Xenibase shows the signing secret **once**; put it in `XENIBASE_WEBHOOK_SECRET`.
A webhook has one URL, and it must be public `https://`, so the one pointing at
your production site (for example on Vercel) does not reach a laptop. Give each
environment its own project (and so its own webhook and secret), or tunnel to
localhost while you develop. Set the secret in the host's environment variables
and redeploy before the first event; if it is lost, **Rotate secret** on the
webhook's page issues a new one.
Events are written in the same transaction as the change, so a rolled-back
change never sends one and a committed one always does. Delivery is **at least
once**, in order: your handler must be safe to run twice.

```ts
// app/api/xenibase/webhook/route.ts
import { createHmac, timingSafeEqual } from "node:crypto";

function verify(secret: string, header: string, rawBody: string, now = Date.now()) {
  const parts = header.split(",").map((kv) => kv.split("="));
  const t = parts.find(([k]) => k === "t")?.[1];
  const sigs = parts.filter(([k]) => k === "v1").map(([, v]) => v);
  if (!t || sigs.length === 0 || Math.abs(now / 1000 - Number(t)) > 300) return false;
  const want = createHmac("sha256", secret).update(`${t}.${rawBody}`).digest("hex");
  return sigs.some((s) => s.length === want.length && timingSafeEqual(Buffer.from(want), Buffer.from(s)));
}

export async function POST(req: Request) {
  const raw = await req.text();                                   // the raw body, before parsing
  if (!verify(process.env.XENIBASE_WEBHOOK_SECRET!, req.headers.get("xenibase-signature") ?? "", raw)) {
    return new Response("bad signature", { status: 401 });
  }
  const event = JSON.parse(raw);
  if (event.type !== "UPDATE" || event.record?.status !== "paid") return new Response("ignored");

  await fulfil(event.record.id, event.id);                        // idempotent: see below
  return new Response("ok");                                      // any 2xx stops retries
}
```

**Make `fulfil` idempotent.** The same event can arrive twice. Read the order's
lines, skip any that already have a `result`, and write the result of each as you
go, with the event's id in the Idempotency-Key:

```ts
async function fulfil(orderId: string, eventId: string) {
  const base = process.env.NEXT_PUBLIC_XENIBASE_URL!;
  const h = { apikey: process.env.XENIBASE_SECRET_KEY!, "Content-Type": "application/json" };

  await fetch(`${base}/data/v1/orders/${orderId}`, {
    method: "PATCH", headers: { ...h, "Idempotency-Key": `fulfilling-${eventId}` },
    body: JSON.stringify({ status: "fulfilling" }),
  });

  const items = await (await fetch(`${base}/data/v1/order_items?where=order_id:eq:${orderId}`, { headers: h })).json();
  let failed = false;
  for (const item of items.data) {
    if (item.result) continue;                                    // already done on an earlier delivery
    const result = await registerOrRenewDomain(item);             // YOUR call to the registrar, with its own idempotency
    failed ||= !result.ok;
    await fetch(`${base}/data/v1/order_items/${item.id}`, {
      method: "PATCH", headers: { ...h, "Idempotency-Key": `item-${item.id}-${eventId}` },
      body: JSON.stringify({ result }),
    });
  }
  await fetch(`${base}/data/v1/orders/${orderId}`, {
    method: "PATCH", headers: { ...h, "Idempotency-Key": `done-${eventId}` },
    body: JSON.stringify({ status: failed ? "failed" : "completed" }),
  });
}
```

**Things that bite here:**

- Respond fast. Return 2xx as soon as you've safely recorded the work; if it
  takes long, acknowledge first and do the work in a queue.
- If the handler errors (non-2xx), Xenibase retries. A broken receiver shows as
  failed deliveries under Database → Webhooks, and the webhook can be paused and
  resumed.
- The registrar call is the one step with a real-world effect. Make it
  idempotent on the registrar's side too (check whether the domain is already
  yours before registering), because a crash between "registered" and "wrote the
  result" would otherwise register twice on retry.
- Don't trust anything in the webhook you could look up: the event says *an
  order changed*; read the order itself before acting.

## 8. Files: receipts

Create a **private** bucket for receipts (Project → Storage, or with the secret
key): `receipts`, allowed type `application/pdf`. Who may read what is decided by
policies on `xb_storage.objects`, written like any other:

```sql
-- A customer may read only files under receipts/<their own user id>/.
CREATE POLICY own_receipts ON xb_storage.objects FOR SELECT TO "<db>_user"
  USING (bucket = 'receipts' AND xb_storage.folder(path, 1) = xb_auth.uid()::text);
```

Your server writes the receipt PDF with the secret key (which skips policies):
path `receipts/<user_id>/<order_id>.pdf`. The customer's page then asks for a
short-lived link:

```ts
const { data: url } = await xb.storage.bucket("receipts").signedUrl(`${user.id}/${orderId}.pdf`, { expiresIn: 600 });
```

A signed link works without a session for its lifetime (here ten minutes), so
keep it short and never store it. Uploads are up to 50 MB in one request; larger
files go in parts (`uploadLarge`).

## 9. Live updates

So a customer watching an order sees *paid → fulfilling → completed* without
refreshing, turn realtime on for the table (Project → Realtime, or as the owner
`SELECT xb_realtime.enable('public.orders');`; the table needs a primary key).
The same RLS policies decide which changes each customer receives: they get only
their own orders.

```ts
const stop = xb.data
  .from("orders")
  .select("id, status")
  .eq("id", orderId)
  .live(({ data }) => render(data));       // runs the query now, and again on every change
// later: stop()
```

Delivery is **at most once**: a change can be missed during a disconnect, which
is why `live()` re-reads the query after a reconnect. If you use the lower-level
channel API, do the same in `onResync`.

## 10. Working day to day

**One project per environment.** Use separate projects for development, staging
and production, each with its own keys and its own `.env`. Never point a local
build at production. For risky schema changes, a **branch** of the database
(Project → Branches) lets you try a migration on a copy of the data first.

**Schema changes go through files in the repository.** Write SQL migrations,
review them like code, apply them to staging, then production. After each one:
regenerate types (`xenibase gen types …`), run the Security advisor, and run
your policy checks.

**Backups** run nightly, and a restore creates a new project, so you can rehearse
a restore without touching production (Project → Backups).

**Use the dashboard to look, not to patch.** The table editor and SQL editor are
real: a change there is a change to production data. Prefer migrations.

**Seeing what the API is doing.** Project → Settings → API shows the **request
log** (method, path, status, latency, role, user, key and IP; kept 7 days) and
this month's **usage** against the plan. Every error response carries a
`request_id`; quote it when you ask for help.

## 11. When something is wrong

Errors look like `{"error":{"code":"…","message":"…","request_id":"req_…"}}`.
The ones you will meet first:

| You see | It means | What to do |
| --- | --- | --- |
| `403 rls_required` | The table has row-level security off (the publishable key can't read it). | Enable RLS and add a policy. Or, for data anyone may read, list it under *Public tables*. |
| Reads return `[]` though rows exist | RLS is on and no policy lets this role see the rows. | Check the policy's role (`TO "<db>_user"`?) and its `USING` test. Use the *Request explorer* to run the same request as that user. |
| `401 invalid_token` / `key_required` | Missing or expired token or key. | Check the `apikey` header; let the client refresh the token. |
| `403 secret_key_in_browser` | The secret key was sent from a web page. | Stop. Replace that key. Move the call to your server. |
| `403 origin_not_allowed` | The page's address isn't in *Allowed origins*. | Add it (Settings → API). |
| `400 redirect_not_allowed` | A sign-in link tried to return to an unlisted address. | Add it under Authentication → *Your app*. |
| `400 filter_required` | An update or delete had no filter or key. | Add one; that is the safety working. |
| `400 query_too_expensive` | The query's planned cost is over the limit. | Add an index or a filter. |
| `409 unique_violation`, `422 check_violation` | A constraint refused the write. | Fix the data; the message names the constraint. |
| `422 idempotency_key_reused` | The same Idempotency-Key with a different request. | Make the key identify the event, and send the same body on a retry. |
| `429 rate_limited` / `over_email_send_rate_limit` | A rate limit (requests per minute, or 30 platform emails an hour). | Back off (`Retry-After`), or set your own SMTP. |
| `503 project_resuming` | A Free project is waking. | Retry after `Retry-After`; don't use Free for production. |
| `503 database_unavailable`, `504 statement_timeout` | The database was unreachable or the query too slow. | Retry; if it persists, quote the `request_id`. |

[Errors](../errors.md) lists every code and whether a retry can help. When you can't
tell *why* a request is refused, reproduce it in **Project → Settings → API →
Request explorer**: it sends the same request as anon, as a chosen user, or as
the service role, and shows the answer, so you can see which of the three roles
sees what.

## 12. Before launch: checklist

Work through this with a second person.

**Access rules**

- [ ] Every table in `public` has RLS enabled and a policy per role that needs
      access. The Security advisor reports nothing at warning level or above.
- [ ] No table is listed under *Public tables* unless it is truly public
      (the price list is; orders are not).
- [ ] Every `SECURITY DEFINER` function is small, validates its input, sets
      `search_path`, and has `EXECUTE` revoked from `PUBLIC` and `anon`.
- [ ] Prices, totals and statuses are set by the database or the server, never
      accepted from the browser.

**Keys and settings**

- [ ] The secret key exists only in server environment variables and the
      password manager. Search the repository and the built bundle for `xb_sec_`.
- [ ] *Allowed origins* lists only the storefront's addresses.
- [ ] *Site URL* and *redirect URLs* are the production ones (no `localhost`).
- [ ] Your own SMTP is set and tested; the email templates read well.
- [ ] Captcha is on for sign-up and code requests if you see abuse
      (Authentication → MFA and captcha); staff accounts require MFA.

**Operations**

- [ ] The project is on a paid plan (Free projects pause).
- [ ] The webhook receiver verifies signatures, is idempotent, and answers
      quickly; a failed delivery pages someone.
- [ ] A backup has been taken and a restore into a new project has been tried.
- [ ] Alerts (Settings → Alerts) go to a channel somebody watches.
- [ ] You know where the request log is and what a `request_id` is for.

## 13. Giving us feedback

You are among the first people building a real product on this, and what you
find is how it improves. When something is wrong, confusing or missing, write
down, for each thing:

1. **What you were trying to do**, in one sentence.
2. **What happened**, with the exact error body (`code`, `message`, and
   especially the **`request_id`**) and the time.
3. **What you expected.**
4. **Where:** the project's `<ref>`, the table or endpoint, the SDK version
   (`@xenibase/client` is `0.1.1`), and whether it was the browser or your server.
5. **How bad:** blocked you, slowed you down, or just surprised you.

Docs that were wrong or unclear count: say which page and which sentence.

**Known gaps today** (so you don't lose time on them):

- **Distance.** A project is served from one region today (`eu-central`). From
  Nigeria each call takes a few hundred milliseconds, so use the patterns in
  section 6 to make fewer of them. A region closer to Nigeria, or a read replica
  there, is planned but needs servers in place first.
- No serverless functions: server logic runs on your own server (section 7).
- Realtime delivery is at most once; re-read after a reconnect (section 9).
- Branches copy the database but not stored files: a branch's files show as
  missing until uploaded again.
- Platform email is limited to 30 a hour per project; use your own SMTP.

## Quick reference

**Base address:** `https://<ref>.api.xenibase.ng`

| Path | For |
| --- | --- |
| `/data/v1/<table>` | Read (`GET`), insert (`POST`), update (`PATCH`), delete (`DELETE`) rows |
| `/data/v1/<table>/<key>` | One row by primary key |
| `/data/v1/rpc/<function>` | Call a database function |
| `/data/v1/batch` | Up to 50 writes in one transaction |
| `/data/v1/openapi.json` | The project's OpenAPI document (secret key) |
| `/auth/v1/signup`, `/signin/password`, `/signin/otp`, `/verify`, `/signout`, `/user` | Sign-up, sign-in and the signed-in user |
| `/storage/v1/object/<bucket>/<path>` | Upload and download |
| `/realtime/v1/websocket` | Live updates (use the client) |

**Headers:** `apikey: xb_pub_…` (browser) or `xb_sec_…` (server);
`Authorization: Bearer <access token>` for a signed-in customer;
`Idempotency-Key: <string>` on writes you may retry.

**Read parameters:** `select=`, `where=column:op:value` (repeatable),
`or=`, `order=column:desc`, `limit=`, `cursor=`, `count=exact`.

**Where things are in the dashboard:**

| To… | Go to |
| --- | --- |
| Turn services on, see keys, address, request log, usage | Project → Settings → API |
| See each table's policies and examples | Project → Settings → API → API docs |
| Test a request as anon / a user / the service role | Project → Settings → API → Request explorer |
| Check for risky settings | Project → Settings → API → Security advisor |
| Sign-in methods, emails, redirect URLs, hooks | Project → Authentication |
| Buckets and files | Project → Storage |
| Turn realtime on for a table | Project → Realtime |
| Webhooks and their delivery results | Database → Webhooks |
| Types for your code | `xenibase gen types --lang ts --project <name>` |

Go deeper: [Backend services](../backend-services.md) (every endpoint and
setting), [the JavaScript client](../sdk/javascript.md), [Next.js](nextjs.md),
[Errors](../errors.md), [Webhooks and scheduled jobs](../webhooks.md) and
[the backend runbook](../backend-runbook.md).
