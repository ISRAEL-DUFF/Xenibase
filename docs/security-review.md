# Security review (spec §7, v1.0.0; V2 surfaces, M16; V3 billing, M27; V4 edge, M37)

Each item of spec §7, how Xenibase enforces it, and what checks it
automatically. "CI" means `.github/workflows/ci.yml` on every push (and
weekly); "live" means xenibase-server's own nightly `isolation_check` operation
on every shared cluster, whose failure raises an alert.

## 7.1 Tenant isolation (shared tier)

Every item below is enforced when a project is created and verified by
`internal/isocheck`, which runs in CI (`test/isolation`) and nightly against
live clusters (`isolation_check`, Settings → Tenant isolation). A failed
check raises the critical `isolation_check_failed` alert.
`TestLiveIsolationCheck` breaks each item on purpose and requires the check
to report it.

| Checklist item | Enforced by | Verified by |
| --- | --- | --- |
| `CONNECT` on each database revoked from `PUBLIC` | `ensureDatabase`: `REVOKE ALL … FROM PUBLIC`, `GRANT CONNECT, TEMPORARY` to the owner only; `postgres` and `template1` revoked at cluster registration | `isocheck.Roles` (every project) and `Cluster` (maintenance databases); `TestTenantIsolation` "cannot connect to B's database", "public has no access"; live probe: tenant A connects to B, `postgres`, `template1` |
| `CREATE` on `public` revoked from `PUBLIC` | `hardenDatabase` | `isocheck.Database` on every project database; isolation suite |
| Project roles `NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS` | `ensureRole` spells every attribute out on `CREATE`/`ALTER ROLE`; console logins are `NOINHERIT` and the same | `isocheck.Roles` (owner, `<db>_console` and `<db>_console_ro`); "role has no dangerous attributes" |
| Not members of `pg_read_all_data`, `pg_write_all_data`, `pg_read_server_files`, `pg_execute_server_program`, or other privileged roles | Roles are never granted; the console login is granted only its project's owner and read-only role, the read-only console login only the read-only role (both `WITH INHERIT FALSE`) | `isocheck.Roles` (membership in any `pg_*` or privileged role; the read-only console login a member of the owner); "not a member of predefined roles"; `TestReadOnlyConsoleCannotBecomeOwner`; live probe |
| Untrusted languages and `plpython3u`, `plperlu`, `dblink`, `postgres_fdw`, `file_fdw`, `adminpack` never available | Project roles cannot create untrusted extensions; the extension API allows only the §7.4 list for the tier (`postgres_fdw` only on dedicated) | `isocheck.Database` (none installed in any project DB); live probe tries each `CREATE EXTENSION`; `TestExtensionsAndMetrics` (API refuses `dblink`, `plpython3u`) |
| etcd (HA, V3 §2.2) | TLS for clients and peers with client certificates required, from a separate etcd CA whose key is sealed with the master key; each HA instance's Patroni has its own client certificate. Patroni's REST API listens on the private network: reads are open, changes (switchover, configuration) need the instance's REST password. The SLA probe login can only connect and run `SELECT 1` (no grants beyond CONNECT). | `TestEtcdCluster` (no client certificate is refused) |
| `pg_hba.conf` accepts only the pooler and control-plane addresses, `scram-sha-256`, `hostssl` where applicable | Bundle: `deploy/compose/shared-pg-hba.conf` names the fixed addresses of xenibase-server, the agent, and both poolers; agent-run clusters and instances: `xenibase-hba.sh` writes `XENIBASE_AGENT_DB_ALLOW` (default: private ranges) at initdb. V3 moves: the moves' own logins (`xenibase_move_<id>`, random password, dropped after the move) may also connect from `XENIBASE_AGENT_MOVE_ALLOW` (default: private ranges; bundle: its Docker network), added by the agent on every start; the bundle's shared cluster allows them from its Docker network | `isocheck.Cluster`: every network rule must be SCRAM (or `reject`/`cert`), name explicit addresses (no `all`, `samenet`, `0.0.0.0/0`), and use `hostssl` outside private ranges; `TestHBAFinding`; `TestClusterConfiguration` |
| `log_statement` off; only `log_min_duration_statement` set | `log_statement=none`, `log_min_duration_statement=5s` in the bundle and agent-run clusters | `isocheck.Cluster` (`log_statement`, `log_duration`, `log_min_duration_statement` not 0) |
| Isolation suite: A cannot connect to B, list B's tables, read B's data through any predefined role, create objects in B, or read server files | — | `test/isolation` (CI) and `isocheck.Probes` (live, two throwaway tenants created with the real create steps): connections, `SET ROLE`, `GRANT`, `ALTER ROLE`, `DROP DATABASE`, `pg_terminate_backend`, `pg_read_file`, `pg_ls_dir`, `COPY … PROGRAM`, `COPY FROM` file, `lo_import`, C functions, superuser-only settings, `pg_stat_activity` query text, `pg_stat_database`, `pg_stat_get_db_*` and `pg_stat_get_backend_*`; `isocheck.Database` flags any of them readable by `PUBLIC` |

The SQL console adds an escape route the spec did not list (`RESET ROLE`
on an admin session). It signs in as a per-project login that inherits
nothing, so `RESET ROLE` leaves no privileges; `TestSQLConsole` tries
`RESET ROLE`, `SET ROLE` to the admin and to a neighbour, `SET SESSION
AUTHORIZATION`, `set_config('role')`, `pg_authid`, `CREATE ROLE`, and
`COPY … PROGRAM`.

## 7.2 Operator authentication

| Item | Implementation | Test |
| --- | --- | --- |
| argon2id password hashes | `internal/auth/password.go` | `TestPasswordHash`, `TestPasswordPolicy` |
| TOTP required, enrolled at first login | The setup wizard creates the owner only after a valid TOTP code; every sign-in needs one | `TestSetupFlow`, `TestTOTPVectors`, `TestVerifyTOTP`, `TestTOTPReplayAndChallengeExpiry` |
| Server-side sessions, `HttpOnly`, `Secure`, `SameSite=Strict`, 12 h idle timeout | `sessions` table; `__Host-` cookies; idle 12 h, at most 7 days | `TestSessionCookieFlags`, `TestReauthAndIdleTimeout`, `TestLoginSessionLogout` |
| CSRF double-submit on mutations | `guard` checks the header against the cookie and the Origin | `TestAuthFlowOverHTTP` |
| Re-authentication for delete, in-place restore, master key rotation, node removal | `reauthRequired` (10 min window); master key rotation is a host CLI (`-rotate-master-key`), which needs the key | `TestReauthAndIdleTimeout`, backups and nodes integration tests |
| Rate limiting and lockout | 10 sign-ins per 5 min per address; account lockout after 5 failures for 15 min, doubling with each further failure up to a day, with an email to the owner; addresses without an account are counted and locked the same way, and signup takes as long for an existing address, so neither tells which addresses are registered | `TestLimiter`, `TestLoginRateLimit`, `TestLockout`, `TestWrongTOTPCountsTowardLockout`, `TestLockoutDoublesAndSaysNothing`, `TestSignupTakesAsLongForAnExistingAddress` |
| Single operator in V1, `operators.role` kept | Setup creates one `owner` | `TestSetupFlow` |
| API tokens (V2 §7.2): hashed, scoped to one org, scopes, project restriction, expiry | `internal/tokens`; the guard's bearer path skips cookies and CSRF; `authz.ScopeFor` per action; platform and session-only routes refuse tokens | `TestRestrictedWriteTokenInCI`, `TestAPITokens`, `TestCLIJobWithRestrictedWriteToken` |
| Tokens follow their user and org | Revoked on removal or disable; disabled while the org is suspended | `TestAPITokens` |
| Token rate limits | 600/min per token, 1,200/min per org | `TestTokenRateLimit` |
| CLI device login | 10-minute codes, approval in a signed-in browser, scopes never widened, the token sealed until collected once | `TestDeviceLogin`, `TestCLIDeviceLogin` |

## 7.3 Secrets

| Item | Implementation | Test |
| --- | --- | --- |
| Secrets encrypted at rest with `XENIBASE_MASTER_KEY` (TOTP secrets, storage credentials, backup key, agent CA key, cluster and instance admin passwords, alert secrets, operation password hand-offs) | AES-256-GCM with per-secret associated data (`internal/crypto`) | `internal/crypto` tests |
| Master key rotation by CLI | `xenibase-server -rotate-master-key` re-encrypts everything in one transaction, then proves the new key alone opens it all | `TestMasterKeyRotation` (including a wrong old key rolling back) |
| Backups encrypted before upload | Per-object file keys wrapped by the backup key (`internal/backupfmt`); WAL-G uses an OpenPGP key derived from it | backup integration tests (`TestBackupDeleteRestoreVerify` checks the object is ciphertext) |
| The operator is made to export the backup key | Setup wizard step 4; a banner until it is confirmed | e2e "fresh install" |

Not stored at all: project passwords (only SCRAM verifiers; a password is
shown once), SQL console query text (audit entries only).

## 7.4 Extension allow-list

Enforced by `POST /projects/{id}/extensions` (per tier, and only extensions
the server has); verified by `TestExtensionsAndMetrics` and, for the
escape-prone ones, by the isolation checks above.

## 7.5 Audit log

Every mutating API call writes a row with operator, action, target, IP, user
agent, time, and outcome (`guard` in `internal/api/security.go`). The table
is append-only in the database itself: triggers refuse `UPDATE`, `DELETE`,
and `TRUNCATE` (`TestAuditLogIsAppendOnly`). It is filterable at `/audit`.
A row the database refuses is retried rather than dropped
([operations](operations.md#the-audit-log)).

What the triggers do not do: stop a compromised xenibase-server. The server
connects as the role that ran the migrations and owns every table (in the
Compose bundle, the metadata database's superuser), so it can
`ALTER TABLE audit_log DISABLE TRIGGER` or drop the table. They stop a bug,
a mistaken statement in the SQL shell, or SQL injection through the
server's queries from changing history, not someone who controls the
server or its database credentials. Making the log hold against that needs
the migrations run by a separate owner and the server connecting as a role
with only `SELECT, INSERT` on `audit_log`; that is not done (it changes every
install's database roles and the upgrade path). Until then, for a log the
server cannot rewrite, ship the server's logs or a logical replica of
`audit_log` somewhere it cannot reach.

## Transport and network

- Clients reach databases only through PgBouncer with TLS required
  (`client_tls_sslmode = require`); certificates from ACME or self-signed.
- Behind the poolers: the edge connects to them with `sslmode=require` by
  default and refuses anything that could fall back to plain text;
  `verify-full` with `XENIBASE_EDGE_POOLER_ROOT_CERT` checks the pooler's
  certificate. PgBouncer connects to the backends with
  `server_tls_sslmode = prefer`, because Xenibase's instances and the waker
  don't serve TLS yet; pooler hosts can set `require` or `verify-full` with a
  CA (`XENIBASE_POOLER_SERVER_TLS_SSLMODE`, `_CA_FILE`) for backends that do.
  Until instances get certificates, keep the poolers and their backends on
  one private network.
- Agents use mTLS with a private CA; the server pins each agent's
  certificate fingerprint and node ID; no work is sent to an agent of
  another major version (`nodes.Compatible`, spec §11.3).
- Webhook alerts can be signed (`X-Xenibase-Signature: t=…,v1=HMAC-SHA256`).
- `/metrics` needs a session or `XENIBASE_METRICS_TOKEN`.

## V2 review (M16)

The surfaces V2 added, how each is enforced, what tests it, and what the
review found and fixed.

| Surface | Enforced by | Verified by |
| --- | --- | --- |
| Permission matrix (V2 §2.3, §2.4) | One `authz.Can`; every route declares an action (`routeRules`), resolved to an organisation before the check; foreign resources answer 404 | `TestEveryRouteDeclaresAnAction`, `TestPermissionMatrix` (every route × every role, platform admin and outsider), `TestMembersCanCreateProjectsSetting` |
| Org scoping in the data layer (V2 §2.6) | Every query on a tenant table filters by `org_id`, or says why it is system-wide (`-- tenant: system`) | `TestTenantQueriesAreScoped` (CI lint), `TestTwoOrgsSeeOnlyTheirOwn` |
| Opaque names (V2 §10.2) | `p_<10 base32>` databases and roles; nothing descriptive written into Postgres; Xenibase's own sessions use generic `application_name`s | `TestOtherTenantsCannotDiscoverProjectNames` |
| Quotas and resource limits (V2 §10.3–10.4) | Plans checked before every create; soft and hard storage locks; `temp_file_limit` and statement reaper on the shared tier; a soft lock applies to SQL jobs too | `TestQuotasAndDedicatedRequests`, `TestStorageLimitLocks`, `TestReaperEndsLongStatements`, `TestJobSQLCannotEscalate` |
| API tokens (V2 §7.2) | Hashed; one org; scopes; project restriction; expiry; refused for disabled or unverified users and suspended orgs; a removed member's tokens fail the membership check; a token made by a token is revoked with it, and an OAuth app's token can't make tokens | `TestAPITokens`, `TestRestrictedWriteTokenInCI`, `TestTokenRateLimit`, `TestDeviceLogin` |
| Outbound requests (V2 §9.1, §10.7) | `internal/outbound`, also for a project's own SMTP server and SMS provider `base_url`: https only (http to allow-listed hosts); every resolved address checked (IPv4-mapped, NAT64 and 6to4 addresses as the IPv4 address they embed); link-local and metadata never, even allow-listed; the request connects to the checked address, without proxies or redirects; per-org rate limits and counters; outbound off per org | `internal/outbound` tests, `TestWebhookDelivery`, `TestScheduledJobs` |
| Break-glass (V2 §2.4) | Platform admin only, step-up auth, at most 4 h, owners emailed, flagged in both audit logs, any owner can end it; it can't invite, change roles or transfer ownership, so it grants nothing that outlives it, and nobody accepts an invitation they sent themselves | `TestSuspensionAndBreakGlass`, `TestBreakGlassCannotGrantMembership` |
| Webhook trigger function (V2 §9.1) | `xenibase.webhook_enqueue()` is `SECURITY DEFINER` with `search_path = pg_catalog, pg_temp`, in a superuser-owned schema with all access revoked from `PUBLIC` (so tenants can't call it or attach it to triggers); a `xenibase` schema anything else owns is dropped first; the worker delivers an outbox row only for one of its own project's webhooks | `TestWebhookDelivery`, `TestWebhooksAcrossCopies` |
| Personal roles across tier moves (V2 §3.5) | Member logins and the read-only role recreated with the same verifiers on promotion, demotion, restore and import | `TestMemberLoginsSurviveRestoreAndPromotion`, `TestDemotionLiveWriter` |
| Running tenant SQL | The console, table editor, schema editor and SQL jobs sign in as the project's console login (`NOINHERIT`, may only `SET ROLE` to the project's roles), never an admin login. Read-only sessions (a `read_only` member, a read-scope token, the table browser's raw filter) sign in as `<db>_console_ro`, which may become only `<db>_ro` | `TestSQLConsole`, `TestJobSQLCannotEscalate`, `TestReadOnlySessionCannotBecomeOwner`, `TestReadOnlyConsoleCannotBecomeOwner` |
| Copying a tenant's database | A restore runs the dump's functions (CHECK constraints, generated columns). Branches, restores, imports and the restore test restore as the console login with `--role` the owner; promotion and demotion through a short-lived login that inherits the project's roles but is no superuser (extensions created first, Xenibase's webhook schema copied on its own) | `TestCopiesDoNotRunAsSuperuser` |

### Found and fixed

- **SQL jobs ran on the superuser's connection** (critical). A job ran as
  `SET LOCAL ROLE <owner>` on an admin connection, so `RESET ROLE` in its
  SQL made it the cluster superuser. Jobs now run through the console
  login.
- **Copies ran the tenant's functions as the superuser** (critical). Every
  restore signed in as the admin and only `SET ROLE` to the owner (or, for
  promotion, demotion and the restore test, not even that), so a function
  in a CHECK constraint could `RESET ROLE` during a branch, restore,
  import, restore test, promotion or demotion. The Postgres documentation
  is explicit that restoring a dump runs code of the source's choosing; an
  import's source is the tenant's own server. Fixed as in the table.
- **NAT64 and 6to4 addresses** reached internal and metadata addresses
  (`64:ff9b::a9fe:a9fe` is 169.254.169.254 on a NAT64 network). They are
  now checked as the IPv4 address they embed.

### Before inviting people outside your own projects

Have someone outside the project review tenant isolation, or pay for a short
penetration test of it (V2 §14 M16): other people's data now depends on it,
and this review found two critical bugs. Start with the copy paths and
anything that runs tenant SQL.

## Atlas review: isolation, the console, the network and the poolers (4.2.4)

The Atlas (`docs/atlas/`) traced these through the code; each is fixed with
a test that failed before.

### Found and fixed

- **A read-only member could read as the owner** (medium-high). Read-only
  console sessions signed in as `<db>_console`, which may `SET ROLE` to the
  owner, so `SELECT set_config('role', '<owner>', true), query_to_xml(…)`
  read rows RLS hides from `<db>_ro`, and tables never granted to it. The
  same went for read-scope tokens and the table browser's raw filter.
  Read-only sessions now sign in as `<db>_console_ro`, granted only
  `<db>_ro` (`WITH INHERIT FALSE, SET TRUE`), created on first use like the
  console login; `isocheck.Roles` flags it if it can become the owner.
- **An import's source address wasn't checked** (medium-high). The server
  and the agent's `/v1/copy` connected to any host, from addresses the
  clusters trust, so an import of `postgres://<owner>:<password>@shared-pg/…`
  got past that project's IP allowlist, and the path probed the private
  network. Import and Supabase migration sources now pass
  `outbound.CheckSource` (public only unless allow-listed; loopback, the
  nodes' and instances' addresses and the platform's networks never), and
  the server and agent connect to the checked address (`hostaddr`).
- **Shared-cluster tenants could watch each other's activity** (low-medium).
  Only `pg_stat_activity` and `pg_stat_get_activity` were hidden;
  `pg_stat_database(_conflicts)` and the `pg_stat_get_db_*` and
  `pg_stat_get_backend_*` functions (every database's commits and backends,
  every backend's database and role) stayed readable. They are revoked from
  `PUBLIC` in each shared project database; migration 00066 queues
  `apply_settings`, which now applies it, for existing projects.
- **Narrowing an IP allowlist left existing connections open** (low-medium).
  PgBouncer checks HBA rules at login only. Saving a list now drops the
  project's clients from addresses it no longer admits (`SHOW CLIENTS`,
  `KILL_CLIENT`), except Xenibase's own logins.
- **The PgBouncer admin console was open to the internet** (low). The
  generated `hba.conf` ended with `host all all 0.0.0.0/0`, which admitted
  database `pgbouncer` too, behind only its password. It now takes logins
  only from loopback and the private ranges (`XENIBASE_POOLER_ADMIN_ALLOW`
  to change them) and rejects them from everywhere else.
- **Nothing checked that allowlists were enforced** (low). A PgBouncer on the
  pre-4.2.4 `pgbouncer.ini` (`auth_type = scram-sha-256`) ignores
  `hba.conf` but reported ready. The arbiter now reads `auth_type` from
  `SHOW CONFIG` on each pooler host (every minute) and counts a host that
  isn't on `hba` as not ready; the alerts check local poolers the same way
  (`pooler_allowlist_off`).
- **Anyone who reached the waker, or a paused project's hostname, could
  wake it** (low). The waker doesn't authenticate; it now accepts only
  loopback, its own machine's addresses, the poolers' addresses and
  `XENIBASE_WAKER_ALLOW`. The edge
  wakes a paused project only for a request with one of its live API keys
  (`project_paused` otherwise).
- **Allowlists cover the poolers only** (low, documented). Instances accept
  SCRAM logins from `XENIBASE_AGENT_DB_ALLOW`, which defaults to every
  private range for an agent installed by hand (the bundle and machines
  Xenibase creates set narrower ones). The agent can't tell which private
  addresses are the platform's, so it now warns at start when the default
  is in use, and `docs/network-access.md` says what to set.
- **One bad route stopped pooler changes everywhere** (low). `Render`
  refused the whole configuration over one invalid row, and `CreateNode`
  accepted addresses (`pg_shared`) it refuses. Node addresses are now
  checked with the renderer's pattern when they are recorded, and a pooler
  sync leaves an invalid route, login or allowlist out (an allowlist with
  its project's routes, so nothing opens up) and raises
  `pooler_config_skipped`, instead of failing every region.

## Atlas review: identity and access (Part E)

The Atlas's identity page traced Xenibase's own accounts, organisations,
tokens and email through the code; each is fixed with a test that failed
before.

### Found and fixed

- **Break-glass could become permanent membership** (high). Under
  break-glass a platform admin could invite their own address as admin and
  accept it, keeping the access after the session, unflagged. Invitations,
  role changes and ownership transfers now answer 403 `break_glass`; nobody
  invites their own address or accepts an invitation they sent
  (`TestBreakGlassCannotGrantMembership`).
- **The lockout revealed accounts and never grew** (medium). Five wrong
  passwords locked a registered address but never an unknown one, the lock
  stayed at 15 minutes, nobody was told, and signup answered an existing
  address 100 times faster. Unknown addresses are now locked the same way,
  the lock doubles to a day, the owner is emailed, and signup hashes either
  way (`TestLockoutDoublesAndSaysNothing`,
  `TestSignupTakesAsLongForAnExistingAddress`).
- **Tokens outlived account recovery, and a session minted them without
  step-up** (high). A stolen cookie could create a year's admin token that
  survived the owner's password reset, sign-out and two-factor reset. A
  password reset and a two-factor reset now revoke every token (a password
  change too, unless the user keeps them), and creating a token or
  approving a device login from a session needs a recent step-up
  (`TestRecoveryRevokesTokens`, `TestTokensNeedARecentStepUp`).
- **An app's tokens outlived disconnecting it** (high). An OAuth token with
  `write` could create a token of its own, which had no app recorded, so
  Disconnect and disabling the app left it working for up to a year.
  Tokens now record the token that created them (`parent_id`), revoking
  one revokes its descendants, and an app's token can't create tokens
  (`TestRevokingATokenRevokesWhatItMade`, `TestTokenMadeByATokenGoesWithIt`,
  `TestOAuthFlowOverHTTP`).
- **A read token could cut its user off** (low). Leaving an organisation
  (`org.view`) and revoking a token took the `read` scope, so a leaked
  read-only token could make its user leave or revoke their admin tokens.
  Both now need `write`, except a token revoking itself
  (`TestReadTokensChangeNothing`).
- **The billing role was lost on re-invitation or transfer** (low). An
  admin could invite a billing member's address with a project, taking
  their billing role and giving them project access, which only owners may
  change. A billing member now stays billing, existing members can't be
  invited, and a transferred project leaves its billing members
  (`TestBillingIsKept`).
- **Invitations were an unlimited mail relay with linked names** (medium).
  Organisation invitations are capped a day per organisation and per
  inviter, and user-chosen names in email are never links
  (`TestInvitationsAreCapped`, `TestInvitationNamesAreNotLinks`,
  `TestInertTextIsNeverALink`).

## V3 review of payments and billing (M27)

> Reviewed when cards and the Flutterwave and iSpend providers existed. They
> are gone: the findings below are kept as the record, and the table is
> updated to the wallet provider, the only webhook now.

Payment webhooks and billing permissions, before money moves through
them for real.

| Surface | Enforced by | Verified by |
| --- | --- | --- |
| Webhook authentication | HMAC-SHA256 over the timestamp and body, compared in constant time, timestamp within 5 minutes. Unknown providers 404; bodies over 1 MB are cut off | `TestWebhookAuthenticationAndForgedEvents`, provider unit tests |
| What a webhook can do | Nothing on its own: its body is a hint. Every payment is looked up again with the provider (`Verify`) before anything is posted, and only a successful naira transaction with an amount counts. A payment below its intent's amount is refused | `providers_test.go` |
| Whose money it is | The provider's record decides: the virtual account the verified transaction was paid into, and the amount the provider reports; the event's own claims are ignored | `TestWebhookAuthenticationAndForgedEvents` |
| Replays and duplicates | Event IDs are unique per provider; payments are unique per provider reference; ledger transactions are keyed. A replayed or re-sent event changes nothing | `TestPartialOverAndDuplicatePayments`, `TestDepositsAreHeldAndDuplicatesAreNoOps` |
| Organisations' billing | Owners and billing members (`org.billing`); members and admins see no invoices. Every invoice, payment, receipt and WHT certificate is looked up with the organisation in the path, so another organisation's ID answers 404 | `TestPermissionMatrix`, `TestBillingAccountsAndRoles` |
| Money-moving admin actions | Platform admin only, and now a fresh step-up (password and code): refunds, recording a manual payment, credit notes, attributing an unmatched event, publishing a price book, an organisation's billing terms | `TestPermissionMatrix`, `reauthRequired` |
| Refund limits | At most the payment less earlier refunds; beyond the org's credit only by reopening the invoices it settled | `TestRefundReopensInvoices` |

### Found and fixed

- **A forged event could credit another organisation's transfer to an
  intent** (high, needs the webhook secret). When the provider's
  verified transaction carried no reference (a bank transfer into a
  virtual account), the intent named in the event body was trusted, so
  whoever could sign a webhook could have a transfer into someone else's
  virtual account credited to their own organisation. With the provider
  that was in use then (Flutterwave),
  signing was a static shared header value. Now only the provider's record
  attributes a payment.
- **Refunds, manual payments and credit notes needed no step-up**
  (medium). A stolen admin session could move money without the second
  factor. They now ask for the password and a code, through a dialog the
  web UI shows for any request the server refuses with `reauth_required`.

### Accepted

- The webhook endpoint is public and unauthenticated requests are cheap to
  refuse; each refusal is logged with the remote address.

## V4 review of the edge (M37)

xenibase-edge is the internet-facing part of backend services: every
request names a project only by its host and proves itself only with that
project's key and, for a user, that project's token. The review read each
path a request can take (keyed, keyless, WebSocket), the feed and report
channel to xenibase-server, and how it reaches the databases, and added fuzz
tests where an input decides which project or data is touched. Plain
`go test` runs the fuzz seeds; `go test -fuzz` runs them further (the
numbers below are from the review).

| Surface | Enforced by | Verified by |
| --- | --- | --- |
| Which project a request is for | Only the host: exactly one label under the API domain, lower-cased, port and a trailing dot ignored; anything else is `unknown_host`. Paths, headers and tokens never name a project | `FuzzRefFromHost` (1.1M inputs), `TestRefFromHost` |
| Keys | The key's SHA-256 is looked up in that project's own key map; a key of another project, a revoked key or a near miss is `invalid_key`. Secret keys are refused from browsers unless the project allows it | `FuzzGatewayIsolation` (1.1M requests over two projects, every host/key/token combination and random ones), `TestEdgeFoundation` |
| User tokens | ES256 only, by kid from that project's JWKS; `aud` must be the project's ref, `role` must be `user`, expiry with a minute's leeway. Both fuzz projects use the same kid, so a token can't pick the other's key by naming it | `FuzzGatewayIsolation` (other project's token, a token signed for the other audience, a service-role token, an expired and an unsigned one), `TestSignVerify` |
| Database access | Each request role is its own login for that project's database only (V4-M32f); `CONNECT` on other databases is revoked; RLS decides rows | `TestDataAPIReads` (the RLS suite), `TestTenantIsolation`, `TestAuthPhoneOAuthMFA` (the hook role) |
| Query safety | Names are quoted with `pgx.Identifier`, JSON path keys as literals with quotes doubled, every value is a parameter; a cost guard (`EXPLAIN`) and statement timeout cap each read | `FuzzReadSQL` (0.9M query strings: nothing outside quotes but the builder's own words, no `;`, comments or a marker value; every parameter used), `TestReadSQL`, `TestDataAPIReads` (the cost guard) |
| Signed file URLs | HMAC-SHA256 with the project's own storage secret over kind, project, bucket, path and expiry, compared in constant time | `FuzzSignedURL` (another project's secret, project, bucket, path or kind, and every one-byte change, refuse), `TestTokens`, `TestStorage` |
| Files served from the API origin | `X-Content-Type-Options: nosniff`, a sandboxing `Content-Security-Policy`, and only images, audio, video, PDF and plain text inline; everything else downloads. Paths with `..`, `.`, empty segments, control characters or invalid UTF-8 are refused | `TestStorage`, `TestPathsAndBuckets`, `TestDetectMIME` |
| Browser access | Allowed origins per project; CORS never allows credentials; the edge sets no cookies | `TestEdgeFoundation` |
| Abuse and cost | Per-IP and per-key limits on keyed and keyless requests, auth limits per endpoint group, realtime sends per connection, render concurrency per process, body limits on every endpoint, a 64 KB WebSocket frame limit (32 KB for broadcast and presence) and bounds on a connection's subscriptions and send queue; spend caps tighten all of them (V4-M37) | `TestEdgeFoundation`, `TestAuthCore`, `TestSpendCapBackendServices`, `TestLimiter` |
| The feed and reports | Signed with `XENIBASE_EDGE_SECRET` and a timestamp; the edge never reads the metadata database | `TestEdgeFoundation` |

### Found and fixed

- **Keyless downloads weren't slowed by a spend cap** (low). Public and
  signed downloads are metered egress, but only keyed requests got the
  capped rate limit. They now share it.

No way was found for a request to reach a project other than the one its
host names, to act as a role its key or token doesn't grant, or to put
request text into SQL outside a parameter, quoted name or literal.

### Accepted

- xenibase-edge sets no read or write timeout on bodies (uploads and
  WebSockets are long), so slow clients hold connections. Put it behind a
  load balancer or TLS terminator that limits connections per address.
- Database errors are returned to the client with their SQLSTATE and
  message (as PostgREST does), which can name tables and constraints the
  caller can already reach.
- An edge process killed mid-report loses up to `ReportEvery` (a few
  seconds) of request counts and logs; usage is under-counted, never
  over-counted.

The external penetration test before GA is scoped in
[pentest-scope.md](pentest-scope.md); its findings are tracked there.

## V4 auth against ASVS 4.0 level 2 (V4.1-M11)

The project auth service (xenibase-edge's `/auth/v1`, `internal/projauth`)
checked against the OWASP Application Security Verification Standard 4.0.3
at level 2: the requirements of V2 (authentication), V3 (sessions), V6
(stored cryptography) and V11 (business logic: anti-automation) that apply
to an API that issues tokens to apps (browser-only requirements, such as
cookie attributes, don't: the edge sets no cookies).

| ASVS | Requirement | How Xenibase meets it | Shown by |
| --- | --- | --- | --- |
| 2.1.1–2.1.3 | Password length, no truncation | At least `password_min_length` characters (default 8, settable 6–128; see Accepted), at most 256 bytes refused rather than cut, any Unicode | `TestAuthCore` |
| 2.1.7 | Breached passwords refused | Not checked (Accepted) | — |
| 2.2.1 | Anti-automation | Per-IP limits per endpoint group (sign-in 30, codes 10 a minute), lockout after five wrong passwords (a minute, doubling), captcha per project, one code a minute per address or number and five an hour and twenty a day per address (counted on requests, whether or not a user has the address, so the answer doesn't enumerate users), at most 5 SMS an hour per number | `TestAuthCore`, `TestAuthCodeBruteForce`, `TestAuthPhoneOAuthMFA`, `TestCodeLimitsDontRevealUsers`, `TestEmailCodesCappedPerAddress` |
| 2.2.3 | Notice of security events | Password changes, MFA changes and sign-ins are in the user's audit log (`xb_auth.audit_log`), and the after-sign-in webhook can notify | `TestAuthCore` |
| 2.4.1, 2.4.4 | Password storage | argon2id, 19 MiB, 2 passes, 16-byte salt (OWASP's minimum); bcrypt hashes brought from Supabase are rehashed at the next sign-in; a missing user costs the same as a wrong password | `TestBcryptPasswords`, `TestSupabaseMigration` |
| 2.5.1–2.5.4 | Recovery | A single-use, 10-minute code or link to the verified address; no hints or questions; a link used for the first time on an unconfirmed address drops a password set before (pre-registration takeover); a sign-up confirmation or invitation keeps only the password its own request set | `TestAuthCore`, `TestSquatterLosesPasswordToOwnersSignup` |
| 2.7.1–2.7.4, 2.8.4 | Out-of-band codes | 6 digits from `crypto/rand`, hashed at rest, 10 minutes, single use, **used up after five wrong tries** (refused even when then right, and still counted for the one-a-minute rule until it expires) | `TestAuthCodeBruteForce` |
| 2.8.1–2.8.5 | TOTP | RFC 6238 SHA-1, 30 s, ±1 step; the matched step is stored and must increase, so a code works once; constant-time compare; five wrong codes in a row lock the factor (a minute, doubling to a day) and the MFA routes have a per-IP limit; MFA factors can only be removed at aal2 | `TestAuthCodeBruteForce`, `TestAuthPhoneOAuthMFA`, `TestTOTPGuessesLockTheFactor` |
| 3.2.1–3.2.3 | Session tokens | A new session and refresh token at every sign-in; access tokens are ES256 JWTs (1 hour by default, 5 minutes to a day); refresh tokens are 256-bit random, stored as SHA-256 | `TestAuthCore` |
| 3.3.1, 3.3.4 | Sign-out | `logout` ends the session (local), all sessions (global) or the others; ended sessions' access tokens are refused at once (the edge checks the session), not at expiry | `TestAuthCore` |
| 3.3.2 | Idle and absolute timeouts | `session_inactivity_seconds` per project (default off); no absolute timeout (see Accepted) | code review (refresh refused after the idle time) |
| 3.3.3 | Sessions after a credential change | **A password change ends every other session** (found in this review, fixed) | `TestPasswordChangeEndsOtherSessions` |
| 3.5.2, 3.5.3 | Token integrity | ES256 only (no `none`, no HS*), key by `kid` from the project's own JWKS, `aud` is the project; refresh-token reuse revokes the whole session family | `FuzzGatewayIsolation`, `TestSignVerify`, `TestAuthCore` |
| 3.7.1 | Re-authentication | Removing a factor needs aal2, and so do enrolling another, verifying a factor enrolled before the first was verified, and changing the password or email, once the user has a verified factor; projects can require aal2 for data and realtime (`mfa_required`) | `TestAuthPhoneOAuthMFA`, `TestAAL1CannotChangeWhatProvesTheUser` |
| 6.2.1–6.2.5 | Approved algorithms | AES-256-GCM for stored secrets (OAuth and SMTP credentials, hook and captcha secrets) under the platform master key with key ids; ES256 signing keys per project; HMAC-SHA256 signed URLs | `TestRoundTrip`, `TestRotation`, `TestMasterKeyRotation`, `TestAuthPhoneOAuthMFA` |
| 6.3.1 | Random values | `crypto/rand` for codes, tokens, salts and keys | code review |
| 6.4.1, 6.4.2 | Key management | Signing keys rotate (the old one verifies until its tokens expire) and a leaked one can be revoked at once; the master key never leaves xenibase-server; edges get only each project's own signing key | `TestAuthCore` (rotation), `TestRevokeSigningKeyWithdrawsItAtOnce` |
| 11.1.2–11.1.4 | Business limits | Request, MAU and SMS caps per plan, a daily SMS cap and country allow-list per project, spend caps | `TestPlanLimitsBackendServices`, `TestAuthPhoneOAuthMFA`, `TestSpendCapBackendServices` |

### Found and fixed

- **A used-up code freed its address at once** (medium). After five wrong
  tries the code was deleted, so the one-code-a-minute rule (which looks
  at the latest code) no longer saw it: a new code, and five more
  guesses, could be had immediately, limited only by the per-IP rate. A
  used-up code now stays, refused, until it expires.
- **A password change left other sessions signed in** (low, ASVS 3.3.3).
  It now ends every session but the one that changed it.

### Accepted

- **Minimum password length 8 by default** (ASVS 2.1.1 asks for 12).
  Projects migrating from Supabase (default 6) keep working; the setting
  goes to 128. Projects that need level 2 should set it to 12.
- **No breached-password check** (2.1.7). It needs either an outbound
  call per sign-up (the edge makes none on that path) or a list kept
  current; projects that need it can check in a before-sign-up hook
  their own way.
- **No idle timeout by default, and no absolute timeout** (3.3.2): mobile
  apps expect long sessions. The idle timeout is a per-project setting;
  an app that needs an absolute limit can sign users out itself.
- **Sign-up says when an address is taken** (`user_already_exists`), as
  GoTrue does, so apps can tell the user; sign-in and recovery don't.

## Dependency audit

CI's `audit` job runs `govulncheck` (Go modules and the standard library
code actually reachable) and `npm audit --audit-level=high` for the web UI
and the e2e harness on every push. At the v1.0.0 review: npm reported 0
vulnerabilities in both; the Go toolchain is 1.25.13.

## Known limitations (accepted for V1)

- Inside one host's Docker network the shared cluster is reached without
  TLS; across hosts, nodes should use a private network (the hba check
  requires `hostssl` for public addresses).
- Operators with shell access to a node can read container environments
  (instance admin passwords, S3 credentials, the WAL-G key), as they can
  read the data itself.
- The audit log is append-only against the server's queries, not against
  the server: it owns the table and could disable the triggers (§7.5).

Sign-off: every §7.1 item is enforced at create and verified automatically
in CI and weekly on live clusters; §7.2–7.5 items are implemented and
covered by the tests named above.
