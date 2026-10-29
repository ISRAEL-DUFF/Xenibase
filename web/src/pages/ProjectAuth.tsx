import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { api, errorMessage, type Project } from "../api/client";
import type { components } from "../api/schema";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  CopyButton,
  Dialog,
  Field,
  FormRow,
  Input,
  KeyValues,
  Page,
  Panel,
  Select,
  SidePanel,
  Spinner,
  Stat,
  Switch,
  Table,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "../components/ui";
import { formatDate, relativeTime } from "../lib/format";
import {
  HooksTab,
  PhoneTab,
  ProvidersTab,
  SecurityTab,
} from "./ProjectAuthMore";
import { useProject } from "./ProjectOverview";

type S = components["schemas"];
type AuthConfig = S["AuthConfig"];
type AuthUser = S["AuthUser"];

const templateKinds: { id: S["AuthTemplatePreview"]["kind"]; label: string }[] =
  [
    { id: "confirmation", label: "Confirm sign-up" },
    { id: "magic_link", label: "Magic link and code" },
    { id: "recovery", label: "Reset password" },
    { id: "invite", label: "Invitation" },
    { id: "email_change", label: "Change email" },
  ];

/** Project → Authentication (V4 §4.9): the app's users, how they sign in,
 * the emails they get, and the keys their tokens are signed with. */
export function ProjectAuthPage() {
  const { data: p } = useProject();
  if (!p) return null;
  return (
    <Page
      title="Authentication"
      description="Your app's users: sign-in by email, phone, WhatsApp and OAuth, second factors, hooks, sessions and tokens."
      testId="project-auth"
    >
      <AuthTabs p={p} />
    </Page>
  );
}

function AuthTabs({ p }: { p: Project }) {
  const svc = useQuery({
    queryKey: ["services", p.id],
    queryFn: () => api.backendServices(p.id),
  });
  const cfg = useQuery({
    queryKey: ["auth-config", p.id],
    queryFn: () => api.authConfig(p.id),
    enabled: !!svc.data?.enabled,
  });
  const [tab, setTab] = useState("users");
  if (svc.isPending) return <Spinner />;
  if (svc.isError) return <Alert>{errorMessage(svc.error)}</Alert>;
  if (!svc.data.enabled)
    return (
      <Panel title="Backend services are off">
        <p className="text-[13px] text-muted">
          Auth is part of backend services.{" "}
          <Link
            to="/projects/$id/settings/api"
            params={{ id: p.id }}
            className="text-accent underline"
          >
            Turn them on in Project Settings → API
          </Link>
          .
        </p>
      </Panel>
    );
  if (cfg.isPending) return <Spinner />;
  if (cfg.isError) return <Alert>{errorMessage(cfg.error)}</Alert>;
  const admin = p.my_role === "admin";
  return (
    <Tabs value={tab} onValueChange={setTab}>
      <TabsList>
        <TabsTrigger value="users">Users</TabsTrigger>
        <TabsTrigger value="settings">Sign-in and sessions</TabsTrigger>
        <TabsTrigger value="emails">Emails</TabsTrigger>
        <TabsTrigger value="phone">Phone</TabsTrigger>
        <TabsTrigger value="providers">Providers</TabsTrigger>
        <TabsTrigger value="security">MFA and captcha</TabsTrigger>
        <TabsTrigger value="hooks">Hooks</TabsTrigger>
        <TabsTrigger value="keys">Signing keys</TabsTrigger>
      </TabsList>
      <TabsContent value="users" className="pt-4">
        <UsersTab p={p} cfg={cfg.data} />
      </TabsContent>
      <TabsContent value="settings" className="pt-4">
        <SettingsTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="emails" className="pt-4">
        <EmailsTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="phone" className="pt-4">
        <PhoneTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="providers" className="pt-4">
        <ProvidersTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="security" className="pt-4">
        <SecurityTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="hooks" className="pt-4">
        <HooksTab p={p} cfg={cfg.data} admin={admin} />
      </TabsContent>
      <TabsContent value="keys" className="pt-4">
        <KeysTab p={p} admin={admin} />
      </TabsContent>
    </Tabs>
  );
}

// ---- Users ----------------------------------------------------------------------

function userStatus(u: AuthUser) {
  if (u.banned_until && new Date(u.banned_until) > new Date())
    return <Badge tone="danger">banned</Badge>;
  if (u.invited_at && !u.email_confirmed_at)
    return <Badge tone="accent">invited</Badge>;
  if (!u.email_confirmed_at) return <Badge tone="warn">unconfirmed</Badge>;
  return <Badge tone="ok">confirmed</Badge>;
}

function UsersTab({ p, cfg }: { p: Project; cfg: AuthConfig }) {
  const [q, setQ] = useState("");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const [open, setOpen] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const users = useQuery({
    queryKey: ["auth-users", p.id, search, page],
    queryFn: () => api.authUsers(p.id, search, page),
  });
  return (
    <div className="flex flex-col gap-4">
      {users.data && (
        <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
          <Stat
            label="Users"
            value={users.data.stats.users}
            testId="auth-stat-users"
          />
          <Stat label="Confirmed" value={users.data.stats.confirmed} />
          <Stat label="Banned" value={users.data.stats.banned} />
          <Stat label="Active sessions" value={users.data.stats.sessions} />
          <Stat
            label="Monthly active"
            value={cfg.monthly_active_users}
            hint="signed in or refreshed this month"
          />
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <form
          className="flex flex-1 gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setPage(1);
            setSearch(q.trim());
          }}
        >
          <Input
            aria-label="Search users"
            placeholder="Search by email, phone or user id"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            className="max-w-md"
          />
          <Button type="submit">Search</Button>
        </form>
        <Button variant="primary" onClick={() => setAdding(true)}>
          Add user
        </Button>
      </div>
      {users.isPending ? (
        <Spinner />
      ) : users.isError ? (
        <Alert>{errorMessage(users.error)}</Alert>
      ) : users.data.items.length === 0 ? (
        <p className="text-[13px] text-muted" data-testid="auth-users-empty">
          {search ? "No users match." : "No users yet."}
        </p>
      ) : (
        <>
          <Table head={["Email", "Status", "Last sign-in", "Created"]}>
            {users.data.items.map((u) => (
              <tr
                key={u.id}
                data-testid="auth-user-row"
                className="cursor-pointer"
                onClick={() => setOpen(u.id)}
              >
                <td className="px-3 py-2 font-medium">
                  {u.email ?? u.phone ?? u.id}
                </td>
                <td className="px-3 py-2">{userStatus(u)}</td>
                <td className="px-3 py-2 text-xs text-muted">
                  {u.last_sign_in_at
                    ? relativeTime(u.last_sign_in_at)
                    : "never"}
                </td>
                <td className="px-3 py-2 text-xs text-muted">
                  {formatDate(u.created_at)}
                </td>
              </tr>
            ))}
          </Table>
          {users.data.total > 50 && (
            <div className="flex items-center justify-end gap-2 text-[13px]">
              <Button disabled={page === 1} onClick={() => setPage(page - 1)}>
                Previous
              </Button>
              <span className="text-muted">
                Page {page} of {Math.ceil(users.data.total / 50)}
              </span>
              <Button
                disabled={page * 50 >= users.data.total}
                onClick={() => setPage(page + 1)}
              >
                Next
              </Button>
            </div>
          )}
        </>
      )}
      {open && <UserPanel p={p} userId={open} onClose={() => setOpen(null)} />}
      {adding && <AddUser p={p} onClose={() => setAdding(false)} />}
    </div>
  );
}

function AddUser({ p, onClose }: { p: Project; onClose: () => void }) {
  const qc = useQueryClient();
  const [email, setEmail] = useState("");
  const [invite, setInvite] = useState(true);
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState(true);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.createAuthUser(p.id, {
        email,
        invite,
        password: invite ? undefined : password,
        email_confirm: invite ? undefined : confirm,
      });
      await qc.invalidateQueries({ queryKey: ["auth-users", p.id] });
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add a user"
      description="Invite someone by email, or create a user with a password."
      testId="add-auth-user"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            form="add-auth-user"
            variant="primary"
            busy={busy}
          >
            {invite ? "Send invitation" : "Create user"}
          </Button>
        </>
      }
    >
      <form
        id="add-auth-user"
        className="flex flex-col gap-3"
        onSubmit={submit}
      >
        <Field label="Email">
          {(id) => (
            <Input
              id={id}
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          )}
        </Field>
        <label className="flex items-center gap-2 text-[13px]">
          <Checkbox
            checked={invite}
            onCheckedChange={(v) => setInvite(v === true)}
          />{" "}
          Send an invitation link (they set up their own sign-in)
        </label>
        {!invite && (
          <>
            <Field label="Password">
              {(id) => (
                <Input
                  id={id}
                  type="password"
                  required
                  autoComplete="new-password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              )}
            </Field>
            <label className="flex items-center gap-2 text-[13px]">
              <Checkbox
                checked={confirm}
                onCheckedChange={(v) => setConfirm(v === true)}
              />{" "}
              Mark the email address as confirmed
            </label>
          </>
        )}
        {err && <Alert>{err}</Alert>}
      </form>
    </Dialog>
  );
}

function UserPanel({
  p,
  userId,
  onClose,
}: {
  p: Project;
  userId: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["auth-user", p.id, userId],
    queryFn: () => api.authUser(p.id, userId),
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const run = async (f: () => Promise<unknown>, close = false) => {
    setBusy(true);
    setErr(null);
    try {
      await f();
      await qc.invalidateQueries({ queryKey: ["auth-users", p.id] });
      if (close) onClose();
      else await q.refetch();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const u = q.data?.user;
  const banned = !!u?.banned_until && new Date(u.banned_until) > new Date();
  return (
    <SidePanel
      open
      onOpenChange={(o) => !o && onClose()}
      title={u?.email ?? "User"}
      description={
        u ? <span className="font-mono text-xs">{u.id}</span> : undefined
      }
      size="large"
      testId="auth-user-panel"
    >
      {q.isPending ? (
        <Spinner />
      ) : q.isError ? (
        <Alert>{errorMessage(q.error)}</Alert>
      ) : (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap gap-2">
            {banned ? (
              <Button
                busy={busy}
                onClick={() =>
                  run(() =>
                    api.updateAuthUser(p.id, userId, { ban_duration: "none" }),
                  )
                }
              >
                Unban
              </Button>
            ) : (
              <>
                <Button
                  busy={busy}
                  onClick={() =>
                    run(() =>
                      api.updateAuthUser(p.id, userId, { ban_duration: "24h" }),
                    )
                  }
                >
                  Ban for 24 hours
                </Button>
                <Button
                  busy={busy}
                  onClick={() =>
                    run(() =>
                      api.updateAuthUser(p.id, userId, {
                        ban_duration: "876000h",
                      }),
                    )
                  }
                >
                  Ban
                </Button>
              </>
            )}
            {!q.data.user.email_confirmed_at && (
              <Button
                busy={busy}
                onClick={() =>
                  run(() =>
                    api.updateAuthUser(p.id, userId, { email_confirm: true }),
                  )
                }
              >
                Confirm email
              </Button>
            )}
            <Button
              busy={busy}
              onClick={() => run(() => api.signOutAuthUser(p.id, userId))}
              data-testid="auth-user-signout"
            >
              Sign out everywhere
            </Button>
            <Button variant="danger" onClick={() => setDeleting(true)}>
              Delete user
            </Button>
          </div>
          {err && <Alert>{err}</Alert>}
          {banned && (
            <Alert tone="warn">
              Banned until {formatDate(q.data.user.banned_until!)}. Their
              sessions end at the next token refresh.
            </Alert>
          )}
          <KeyValues
            items={[
              ["Status", userStatus(q.data.user)],
              ["Created", formatDate(q.data.user.created_at)],
              [
                "Last sign-in",
                q.data.user.last_sign_in_at
                  ? formatDate(q.data.user.last_sign_in_at)
                  : "never",
              ],
              [
                "User metadata",
                <code key="m" className="font-mono text-xs break-all">
                  {JSON.stringify(q.data.user.user_metadata)}
                </code>,
              ],
              [
                "App metadata",
                <code key="a" className="font-mono text-xs break-all">
                  {JSON.stringify(q.data.user.app_metadata)}
                </code>,
              ],
            ]}
          />
          <Panel title="Sessions" testId="auth-user-sessions">
            {q.data.sessions.length === 0 ? (
              <p className="text-[13px] text-muted">Not signed in anywhere.</p>
            ) : (
              <Table head={["Device", "Signed in", "Last refreshed"]}>
                {q.data.sessions.map((s) => (
                  <tr key={s.id}>
                    <td
                      className="max-w-64 truncate px-3 py-2 text-xs"
                      title={s.user_agent ?? ""}
                    >
                      {s.user_agent ?? "—"} {s.ip ? `· ${s.ip}` : ""}
                    </td>
                    <td className="px-3 py-2 text-xs text-muted">
                      {relativeTime(s.created_at)}
                    </td>
                    <td className="px-3 py-2 text-xs text-muted">
                      {relativeTime(s.refreshed_at)}
                    </td>
                  </tr>
                ))}
              </Table>
            )}
          </Panel>
          <Panel title="Recent activity" testId="auth-user-audit">
            {q.data.audit.length === 0 ? (
              <p className="text-[13px] text-muted">Nothing yet.</p>
            ) : (
              <ul className="flex flex-col divide-y divide-line text-[13px]">
                {q.data.audit.map((a) => (
                  <li key={a.id} className="flex justify-between gap-3 py-1.5">
                    <span className="font-mono text-xs">{a.action}</span>
                    <span
                      className="text-xs text-muted"
                      title={formatDate(a.at)}
                    >
                      {relativeTime(a.at)} {a.ip ? `· ${a.ip}` : ""}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
          <Dialog
            open={deleting}
            onOpenChange={setDeleting}
            title="Delete this user?"
            description="Their account, sessions and sign-in methods are deleted; your tables' rows that reference them follow their foreign keys (ON DELETE CASCADE removes them)."
            footer={
              <>
                <Button variant="ghost" onClick={() => setDeleting(false)}>
                  Cancel
                </Button>
                <Button
                  variant="danger"
                  busy={busy}
                  onClick={() =>
                    run(() => api.deleteAuthUser(p.id, userId), true)
                  }
                >
                  Delete user
                </Button>
              </>
            }
          />
        </div>
      )}
    </SidePanel>
  );
}

// ---- Sign-in and sessions -----------------------------------------------------------

function SettingsTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const qc = useQueryClient();
  const st = cfg.settings;
  const [site, setSite] = useState(st.site_url ?? "");
  const [redirects, setRedirects] = useState(
    (st.redirect_urls ?? []).join("\n"),
  );
  const [wildcards, setWildcards] = useState(!!st.allow_wildcard_redirects);
  const [signup, setSignup] = useState(!!st.signup_enabled);
  const [confirm, setConfirm] = useState(!!st.email_confirm);
  const [magic, setMagic] = useState(!!st.magic_link_enabled);
  const [layout, setLayout] = useState(st.email_layout !== false);
  const [minLen, setMinLen] = useState(st.password_min_length ?? 8);
  const [mixed, setMixed] = useState(!!st.password_require_mixed);
  const [ttl, setTtl] = useState(st.access_token_ttl ?? 3600);
  const [maxAge, setMaxAge] = useState(st.session_max_seconds ?? 0);
  const [idle, setIdle] = useState(st.session_inactivity_seconds ?? 0);
  const [single, setSingle] = useState(!!st.single_session);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    setSaved(false);
    try {
      await api.updateAuthConfig(p.id, {
        settings: {
          site_url: site.trim(),
          redirect_urls: redirects
            .split(/\s+/)
            .map((r) => r.trim())
            .filter(Boolean),
          allow_wildcard_redirects: wildcards,
          signup_enabled: signup,
          email_confirm: confirm,
          magic_link_enabled: magic,
          email_layout: layout,
          password_min_length: minLen,
          password_require_mixed: mixed,
          access_token_ttl: ttl,
          session_max_seconds: maxAge,
          session_inactivity_seconds: idle,
          single_session: single,
        },
      });
      await qc.invalidateQueries({ queryKey: ["auth-config", p.id] });
      setSaved(true);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const sw = (
    label: string,
    v: boolean,
    set: (v: boolean) => void,
    description?: string,
  ) => (
    <FormRow label={label} description={description}>
      <Switch
        aria-label={label}
        checked={v}
        onCheckedChange={set}
        disabled={!admin}
      />
    </FormRow>
  );
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={save}
      data-testid="auth-settings"
    >
      <Panel title="Your app">
        <KeyValues
          items={[
            [
              "Auth API",
              <span
                key="u"
                className="inline-flex items-center gap-2 font-mono text-xs"
              >
                {cfg.auth_url}
                <CopyButton value={cfg.auth_url} />
              </span>,
            ],
          ]}
        />
        <FormRow
          label="Site URL"
          description="Where email links go when a request names no redirect; pages under it are allowed redirects."
        >
          <Input
            aria-label="Site URL"
            placeholder="https://app.example.com"
            value={site}
            onChange={(e) => setSite(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
        <FormRow
          label="Redirect URLs"
          description="Other addresses email links may return to, one per line, matched exactly."
        >
          <textarea
            aria-label="Redirect URLs"
            className="min-h-16 w-full rounded-md border border-line-strong bg-surface-2 p-2.5 font-mono text-xs"
            value={redirects}
            onChange={(e) => setRedirects(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
        {sw(
          "Allow wildcards in redirect URLs",
          wildcards,
          setWildcards,
          '"*" matches within a path segment, "**" anything.',
        )}
      </Panel>
      <Panel title="Sign-in methods">
        {sw("Allow new sign-ups", signup, setSignup)}
        {sw(
          "Confirm email addresses",
          confirm,
          setConfirm,
          "Users confirm by link or code before signing in with a password.",
        )}
        {sw("Magic links and email codes", magic, setMagic)}
        {sw(
          "Clean email layout",
          layout,
          setLayout,
          "Your confirmation, sign-in and password-reset emails are laid out under your project's name, with a button for the link. Turn off to send your template's text exactly as written.",
        )}
        <FormRow label="Minimum password length">
          <Input
            type="number"
            min={6}
            max={128}
            aria-label="Minimum password length"
            value={minLen}
            onChange={(e) => setMinLen(Number(e.target.value))}
            disabled={!admin}
          />
        </FormRow>
        {sw("Require letters and digits", mixed, setMixed)}
      </Panel>
      <Panel title="Sessions and tokens">
        <FormRow
          label="Access token lifetime (seconds)"
          description="300 to 86400. Bans and sign-outs take effect within it."
        >
          <Input
            type="number"
            min={300}
            max={86400}
            aria-label="Access token lifetime"
            value={ttl}
            onChange={(e) => setTtl(Number(e.target.value))}
            disabled={!admin}
          />
        </FormRow>
        <FormRow
          label="Maximum session length (seconds)"
          description="0 for no limit, otherwise at least 3600."
        >
          <Input
            type="number"
            min={0}
            aria-label="Maximum session length"
            value={maxAge}
            onChange={(e) => setMaxAge(Number(e.target.value))}
            disabled={!admin}
          />
        </FormRow>
        <FormRow
          label="Sign out after inactivity (seconds)"
          description="0 for never, otherwise at least 600."
        >
          <Input
            type="number"
            min={0}
            aria-label="Inactivity timeout"
            value={idle}
            onChange={(e) => setIdle(Number(e.target.value))}
            disabled={!admin}
          />
        </FormRow>
        {sw(
          "One session per user",
          single,
          setSingle,
          "Signing in ends the user's other sessions.",
        )}
      </Panel>
      {admin && (
        <div className="flex items-center justify-end gap-3">
          {saved && (
            <span className="text-xs text-muted">
              Saved; the edge applies it within seconds.
            </span>
          )}
          {err && <Alert>{err}</Alert>}
          <Button type="submit" variant="primary" busy={busy}>
            Save
          </Button>
        </div>
      )}
    </form>
  );
}

// ---- Emails ----------------------------------------------------------------------------

function EmailsTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  return (
    <div className="flex flex-col gap-4">
      <Panel title="Sending" testId="auth-email-sending">
        {cfg.email.own_smtp ? (
          <p className="text-[13px]">
            Sent through your SMTP server ({cfg.smtp?.host}).
          </p>
        ) : (
          <Alert tone="warn" title="Platform email: for development">
            Up to {cfg.email.platform_per_hour} emails an hour go through
            Xenibase's email ({cfg.email.platform_left} left this hour). Set up
            your own SMTP server before launch.
          </Alert>
        )}
        <p className="mt-2 text-xs text-muted">
          Last 24 hours: {cfg.email.sent_24h} sent, {cfg.email.failed_24h}{" "}
          failed.
        </p>
      </Panel>
      <SMTPPanel p={p} cfg={cfg} admin={admin} />
      <TemplatesPanel p={p} cfg={cfg} admin={admin} />
    </div>
  );
}

function SMTPPanel({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const qc = useQueryClient();
  const [host, setHost] = useState(cfg.smtp?.host ?? "");
  const [port, setPort] = useState(cfg.smtp?.port ?? 587);
  const [user, setUser] = useState(cfg.smtp?.username ?? "");
  const [password, setPassword] = useState("");
  const [from, setFrom] = useState(cfg.smtp?.from ?? "");
  const [tls, setTls] = useState<NonNullable<S["AuthSMTP"]["tls"]>>(
    cfg.smtp?.tls ?? "starttls",
  );
  const [testTo, setTestTo] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const smtp = (): S["AuthSMTP"] => ({
    host,
    port,
    username: user,
    password,
    from,
    tls,
  });
  const run = async (f: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    setMsg(null);
    try {
      await f();
      await qc.invalidateQueries({ queryKey: ["auth-config", p.id] });
      setMsg({ ok: true, text: ok });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Your SMTP server"
      description="Emails come from your domain, without Xenibase's hourly limit."
      testId="auth-smtp"
    >
      <form
        className="flex flex-col"
        onSubmit={(e) => {
          e.preventDefault();
          void run(
            () => api.updateAuthConfig(p.id, { smtp: smtp() }),
            "Saved.",
          );
        }}
      >
        <FormRow label="Host">
          <Input
            aria-label="SMTP host"
            value={host}
            onChange={(e) => setHost(e.target.value)}
            disabled={!admin}
            required
          />
        </FormRow>
        <FormRow label="Port and security">
          <div className="flex gap-2">
            <Input
              type="number"
              aria-label="SMTP port"
              className="w-28"
              value={port}
              onChange={(e) => setPort(Number(e.target.value))}
              disabled={!admin}
            />
            <Select
              aria-label="SMTP security"
              value={tls}
              onChange={(e) => setTls(e.target.value as typeof tls)}
              disabled={!admin}
            >
              <option value="starttls">STARTTLS (587)</option>
              <option value="tls">TLS (465)</option>
              <option value="none">None (local relay only)</option>
            </Select>
          </div>
        </FormRow>
        <FormRow label="Username">
          <Input
            aria-label="SMTP username"
            value={user}
            onChange={(e) => setUser(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
        <FormRow
          label="Password"
          description={
            cfg.smtp ? "Leave empty to keep the saved one." : undefined
          }
        >
          <Input
            type="password"
            aria-label="SMTP password"
            autoComplete="off"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
        <FormRow label="From">
          <Input
            aria-label="From address"
            placeholder="My App <auth@example.com>"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
            disabled={!admin}
            required
          />
        </FormRow>
        {admin && (
          <div className="flex flex-wrap items-center justify-end gap-2 pt-3">
            <Input
              aria-label="Send a test to"
              placeholder="you@example.com"
              className="max-w-56"
              value={testTo}
              onChange={(e) => setTestTo(e.target.value)}
            />
            <Button
              type="button"
              busy={busy}
              disabled={!testTo}
              onClick={() =>
                run(
                  () => api.testAuthSMTP(p.id, { to: testTo, smtp: smtp() }),
                  `Test email sent to ${testTo}.`,
                )
              }
            >
              Send test
            </Button>
            {cfg.smtp && (
              <Button
                type="button"
                variant="ghost"
                busy={busy}
                onClick={() =>
                  run(
                    () => api.updateAuthConfig(p.id, { clear_smtp: true }),
                    "Back to the platform's email.",
                  )
                }
              >
                Remove
              </Button>
            )}
            <Button type="submit" variant="primary" busy={busy}>
              Save
            </Button>
          </div>
        )}
        {msg && (
          <div className="mt-3">
            <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>
          </div>
        )}
      </form>
    </Panel>
  );
}

function TemplatesPanel({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const qc = useQueryClient();
  const [kind, setKind] =
    useState<(typeof templateKinds)[number]["id"]>("confirmation");
  const current = cfg.templates[kind] ?? cfg.default_templates[kind];
  const [subject, setSubject] = useState(current?.subject ?? "");
  const [body, setBody] = useState(current?.body ?? "");
  const [preview, setPreview] = useState<S["AuthEmailTemplate"] | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const pick = (k: typeof kind) => {
    setKind(k);
    const t = cfg.templates[k] ?? cfg.default_templates[k];
    setSubject(t?.subject ?? "");
    setBody(t?.body ?? "");
    setPreview(null);
    setMsg(null);
  };
  const run = async (f: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    setMsg(null);
    try {
      await f();
      await qc.invalidateQueries({ queryKey: ["auth-config", p.id] });
      setMsg({ ok: true, text: ok });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Email templates"
      description="Plain text, with {{.Code}}, {{.Link}}, {{.Email}} and {{.SiteURL}}."
      testId="auth-templates"
    >
      <div className="flex flex-col gap-3">
        <Select
          aria-label="Template"
          value={kind}
          onChange={(e) => pick(e.target.value as typeof kind)}
        >
          {templateKinds.map((t) => (
            <option key={t.id} value={t.id}>
              {t.label}
              {cfg.templates[t.id] ? " (customised)" : ""}
            </option>
          ))}
        </Select>
        <Input
          aria-label="Subject"
          value={subject}
          onChange={(e) => setSubject(e.target.value)}
          disabled={!admin}
        />
        <textarea
          aria-label="Body"
          className="min-h-40 w-full rounded-md border border-line-strong bg-surface-2 p-2.5 font-mono text-xs"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          disabled={!admin}
        />
        <div className="flex flex-wrap justify-end gap-2">
          <Button
            busy={busy}
            onClick={async () => {
              setMsg(null);
              try {
                setPreview(
                  await api.previewAuthTemplate(p.id, { kind, subject, body }),
                );
              } catch (e) {
                setMsg({ ok: false, text: errorMessage(e) });
              }
            }}
          >
            Preview
          </Button>
          {admin && cfg.templates[kind] && (
            <Button
              variant="ghost"
              busy={busy}
              onClick={() =>
                run(
                  () =>
                    api.updateAuthConfig(p.id, {
                      templates: { [kind]: { subject: "", body: "" } },
                    }),
                  "Back to the default.",
                )
              }
            >
              Use the default
            </Button>
          )}
          {admin && (
            <Button
              variant="primary"
              busy={busy}
              onClick={() =>
                run(
                  () =>
                    api.updateAuthConfig(p.id, {
                      templates: { [kind]: { subject, body } },
                    }),
                  "Saved.",
                )
              }
            >
              Save template
            </Button>
          )}
        </div>
        {msg && <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>}
        {preview && (
          <div
            className="rounded-md border border-line bg-surface-2 p-3 text-[13px]"
            data-testid="template-preview"
          >
            <div className="mb-2 font-medium">{preview.subject}</div>
            <pre className="font-sans whitespace-pre-wrap">{preview.body}</pre>
          </div>
        )}
      </div>
    </Panel>
  );
}

// ---- Signing keys --------------------------------------------------------------------

function KeysTab({ p, admin }: { p: Project; admin: boolean }) {
  const qc = useQueryClient();
  const keys = useQuery({
    queryKey: ["signing-keys", p.id],
    queryFn: () => api.signingKeys(p.id),
  });
  const [confirm, setConfirm] = useState(false);
  const [revoking, setRevoking] = useState<S["SigningKey"] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const revoke = async (k: S["SigningKey"]) => {
    setBusy(true);
    setErr(null);
    try {
      await api.revokeSigningKey(p.id, k.id);
      await qc.invalidateQueries({ queryKey: ["signing-keys", p.id] });
      setRevoking(null);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const rotate = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.rotateSigningKey(p.id);
      await qc.invalidateQueries({ queryKey: ["signing-keys", p.id] });
      setConfirm(false);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Signing keys"
      description="Access tokens are ES256 JWTs. Your servers verify them with the public keys at /auth/v1/.well-known/jwks.json; no secret is shared."
      testId="auth-signing-keys"
      actions={
        admin && <Button onClick={() => setConfirm(true)}>Rotate key</Button>
      }
    >
      {err && <Alert>{err}</Alert>}
      {keys.isPending ? (
        <Spinner />
      ) : keys.isError ? (
        <Alert>{errorMessage(keys.error)}</Alert>
      ) : (
        <Table
          head={[
            "Key id",
            "Status",
            "Created",
            "Verifies until",
            ...(admin ? [""] : []),
          ]}
        >
          {keys.data.items.map((k) => (
            <tr key={k.id} data-testid="signing-key-row">
              <td className="px-3 py-2 font-mono text-xs">{k.kid}</td>
              <td className="px-3 py-2">
                <Badge tone={k.status === "active" ? "ok" : "muted"}>
                  {k.status === "active" ? "signing" : k.status}
                </Badge>
              </td>
              <td className="px-3 py-2 text-xs text-muted">
                {formatDate(k.created_at)}
              </td>
              <td className="px-3 py-2 text-xs text-muted">
                {k.verify_until ? formatDate(k.verify_until) : "—"}
              </td>
              {admin && (
                <td className="px-3 py-2 text-right">
                  <Button variant="ghost" onClick={() => setRevoking(k)}>
                    Revoke
                  </Button>
                </td>
              )}
            </tr>
          ))}
        </Table>
      )}
      <Dialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Rotate the signing key?"
        description="New tokens are signed with a new key. The current key keeps verifying for a day, so signed-in users aren't signed out; servers that cache the public keys should refresh them."
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirm(false)}>
              Cancel
            </Button>
            <Button variant="primary" busy={busy} onClick={() => void rotate()}>
              Rotate
            </Button>
          </>
        }
      />
      <Dialog
        open={revoking !== null}
        onOpenChange={(o) => !o && setRevoking(null)}
        title="Revoke this signing key?"
        description={
          revoking?.status === "active"
            ? "For a key that leaked. Tokens it signed stop working within seconds, so every user is signed out, and a new key signs from now on."
            : "For a key that leaked. Tokens it signed stop working within seconds instead of when they expire."
        }
        footer={
          <>
            <Button variant="ghost" onClick={() => setRevoking(null)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              busy={busy}
              onClick={() => revoking && void revoke(revoking)}
            >
              Revoke
            </Button>
          </>
        }
      />
    </Panel>
  );
}
