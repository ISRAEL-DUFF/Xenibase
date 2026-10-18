# OAuth for third-party apps

A third-party app can ask a Xenibase user to connect one of their
organisations to it, with explicit, scoped, revocable consent, instead of
the user pasting an API token into the app. The flow is OAuth 2.0's
authorization code grant (RFC 6749 section 4.1) with PKCE (RFC 7636),
which every client must use. What the app ends up with is an ordinary
Xenibase API token (`xb_…`, see [the CLI guide](cli.md#tokens)): it acts
in one organisation, with the scopes the user granted, optionally only on
some projects, until it expires or someone revokes it. Everything a token
can do, its rate limits, and how it is revoked are the same as for any
other token. [Taskiem](integrations/README.md) is the first app to use
this.

## 1. Register the app

A platform admin registers the app once per Xenibase server, from a
browser session (platform routes don't take API tokens):

```http
POST /api/v1/admin/oauth/clients
{
  "name": "Taskiem",
  "redirect_uris": ["https://app.taskiem.com/connections/xenibase/callback"],
  "scopes_allowed": ["read", "write"],
  "confidential": true
}
```

```json
{
  "client": {
    "id": "…", "client_id": "xbapp_4Qf…", "name": "Taskiem", "confidential": true,
    "redirect_uris": ["https://app.taskiem.com/connections/xenibase/callback"],
    "scopes_allowed": ["read", "write"], "created_at": "…"
  },
  "client_secret": "xbc_…"
}
```

- `name` is what users see on the consent page and next to the token on
  their Account page.
- `redirect_uris` must be absolute `https://` URLs without a fragment
  (`http://localhost` and `http://127.0.0.1` are allowed for
  development). An authorize request must match one of them exactly,
  including any query string.
- `scopes_allowed` is the most the app may ask for; `admin` has to be
  listed explicitly. Users can always grant less.
- The `client_secret` is shown once. A **public client**
  (`"confidential": false`) gets none and relies on PKCE alone: use that
  for a native or single-page app that can't keep a secret.

`GET /api/v1/admin/oauth/clients` lists the apps;
`DELETE /api/v1/admin/oauth/clients/{client_id}` disables one and revokes
every token it was issued.

## 2. Send the user to the consent page

Generate a PKCE verifier (43 to 128 characters of `A-Z a-z 0-9 - . _ ~`)
and its S256 challenge, and a `state` you can check on the way back:

```js
const verifier = base64url(crypto.getRandomValues(new Uint8Array(32)));
const challenge = base64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
```

Then open, in the user's browser:

```
https://<xenibase>/oauth/authorize
  ?client_id=xbapp_4Qf…
  &redirect_uri=https%3A%2F%2Fapp.taskiem.com%2Fconnections%2Fxenibase%2Fcallback
  &scope=read%20write
  &state=<random>
  &code_challenge=<challenge>
  &code_challenge_method=S256
```

- `scope` is space-separated `read`, `write`, `admin` (`write` includes
  `read`; `admin` requires `write`). Left out, the app's allowed scopes
  are requested.
- `code_challenge_method` must be `S256`; `plain` is refused.

A signed-out user logs in (with their second factor) and lands on the
consent page afterwards. The page names the app, lets the user choose the
organisation, narrow the scopes, restrict the token to some projects and
pick an expiry (7 to 365 days, 90 by default), then approve or deny. The
user can only grant what they themselves can do.

The browser comes back to your `redirect_uri` with:

- `code` and `state` when approved. The code is single use and lasts 10
  minutes.
- `error=access_denied` (and `state`) when denied.

A bad `client_id` or `redirect_uri`, a scope the app may not ask for, or
a missing challenge shows an error on the consent page instead; nothing
is redirected (RFC 6749 section 4.1.2.1).

## 3. Exchange the code

From your server (never the browser, for a confidential client):

```http
POST /api/v1/oauth/token
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code
&code=<code>
&redirect_uri=https%3A%2F%2Fapp.taskiem.com%2Fconnections%2Fxenibase%2Fcallback
&client_id=xbapp_4Qf…
&client_secret=xbc_…
&code_verifier=<verifier>
```

The client credentials may instead go in an HTTP Basic `Authorization`
header, and the body may be JSON. The answer:

```json
{
  "access_token": "xb_…",
  "token_type": "Bearer",
  "expires_in": 7776000,
  "scope": "read write",
  "org_id": "…",
  "project_ids": null,
  "token_id": "…"
}
```

- `access_token` is the API token, shown once. Store it like a password.
- `expires_in` is in seconds. There is no refresh token: when the token
  expires, ask the user to connect again.
- `org_id` is the organisation the user chose; `project_ids` lists the
  projects the token is restricted to, or `null` for every project the
  user can access there. Keep both: `GET /api/v1/orgs` and
  `GET /api/v1/projects?org=…` with the token answer the same.

Errors are in OAuth's shape, `{"error": "…", "error_description": "…"}`:

| `error` | Status | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | A parameter is missing or malformed (the `code_verifier`, the body). |
| `invalid_client` | 401 | Unknown or disabled `client_id`, wrong `client_secret`, or a secret sent for a public client. |
| `invalid_grant` | 400 | The code is unknown, expired, already used, issued to another client, the `redirect_uri` differs, or the `code_verifier` doesn't match. |
| `unsupported_grant_type` | 400 | Only `authorization_code`. |
| `slow_down` | 429 | Too many attempts from one address; `Retry-After` is sent. |
| `server_error` | 500 | A fault on Xenibase's side. |

A code works **once**. Presenting it again fails with `invalid_grant`
and revokes the token the first exchange issued (RFC 6749 section
4.1.2), so an attacker who caught a code after you used it gets nothing
and you notice.

## 4. Use the token

```http
GET /api/v1/projects?org=<org_id>
Authorization: Bearer xb_…
```

The token does what the user granted and never more than the user can
do: it loses access when they leave the organisation, when their role is
lowered, when the organisation is suspended, and when it expires.
Destructive actions need the `admin` scope and a typed `confirm` field
(see [the CLI guide](cli.md#tokens)). Rate limits are the usual 600
requests a minute per token and 1,200 per organisation
([error codes](errors.md)).

A `401` with code `invalid_token` means the connection is broken: the
token expired or was revoked. Mark the connection as needing attention
and ask the user to connect again; there is nothing to retry.

## 5. Revoke

Your app revokes a token it no longer needs (the user disconnected from
your side) with RFC 7009's endpoint, authenticating as the client:

```http
POST /api/v1/oauth/revoke
Authorization: Basic base64(client_id:client_secret)
Content-Type: application/x-www-form-urlencoded

token=xb_…
```

It answers `200` whether or not the token was still valid, or was ever
yours (`401 invalid_client` only for a bad client credential).

Users revoke from the other side: **Account → Connected apps →
Disconnect**, where each token shows your app's name, the organisation,
the scopes and projects, and when it was last used. Organisation owners
and admins see and can revoke it under **Organisation → API tokens**, and
a platform admin disabling your app revokes every token at once. In every
case the token answers `401 invalid_token` from then on.

## What is audited

Every consent (`oauth.authorize`, denied or approved), every token issued
(`oauth.token_issue`) and every revocation through the endpoint
(`oauth.token_revoke`) is in the organisation's audit log, alongside the
token's own `token.create` and `token.revoke` entries.
