import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage } from "../api/client";
import { Alert, Badge, Button, Dialog, Field, FormRow, Input, Page, Panel, Section, Table, PageSkeleton } from "../components/ui";
import { RecoveryCodes, TotpEnrol } from "../components/Enrol";
import { TokensCard } from "../components/Tokens";
import { formatDate, relativeTime } from "../lib/format";
import { setCurrentOrg } from "../lib/org";
import { twoFactorQuery } from "../lib/twofactor";
import { refreshSession, sessionQuery } from "../lib/session";

/** Profile, password, two-factor, sessions, tokens, and invitations (V2 §13). */
export function AccountPage() {
  const { data: session } = useQuery(sessionQuery);
  if (!session?.user) return <PageSkeleton />;
  return (
    <Page title="Account" description="Your profile, how you sign in, and the tokens that act for you.">
      <div className="flex max-w-4xl flex-col gap-8">
        <InvitationsCard />
        <Section title="Profile">
          <ProfileCard />
        </Section>
        <Section title="Security" description="Your password, and an optional second step at sign-in.">
          <PasswordCard />
          <TwoFactorCard />
        </Section>
        <Section title="Sessions" description="Browsers signed in as you. Revoke any you don't recognise.">
          <SessionsCard />
        </Section>
        <Section title="Access tokens">
          <TokensCard />
        </Section>
      </div>
    </Page>
  );
}

function InvitationsCard() {
  const q = useQuery({ queryKey: ["me", "invitations"], queryFn: api.myInvitations });
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [err, setErr] = useState<string | null>(null);
  if (!q.data?.items.length) return null;
  return (
    <Panel title="Invitations" description="Organisations that invited you." tone="warn">
      <div id="invitations" className="flex flex-col gap-2">
        {q.data.items.map((i) => (
          <div key={i.id} className="flex flex-wrap items-center justify-between gap-2 text-sm">
            <span>
              <strong>{i.org_name ?? "Xenibase"}</strong> — {i.role}
              {i.projects.length > 0 && `, ${i.projects.length} project${i.projects.length === 1 ? "" : "s"}`}, from {i.invited_by}
            </span>
            <Button
              variant="primary"
              className="text-xs"
              onClick={async () => {
                setErr(null);
                try {
                  const r = await api.acceptMyInvitation(i.id);
                  await qc.invalidateQueries();
                  if (r.org_id) {
                    setCurrentOrg(r.org_id);
                    await navigate({ to: "/projects" });
                  }
                } catch (e) {
                  setErr(errorMessage(e));
                }
              }}
            >
              Accept
            </Button>
          </div>
        ))}
        {err && <Alert>{err}</Alert>}
      </div>
    </Panel>
  );
}

function ProfileCard() {
  const { data: session } = useQuery(sessionQuery);
  const qc = useQueryClient();
  const [name, setName] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const value = name ?? session?.user?.name ?? "";
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.updateMe({ name: value });
      setName(null);
      await refreshSession(qc);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      footer={
        <>
          <Button disabled={name === null} onClick={() => setName(null)}>
            Cancel
          </Button>
          <Button type="submit" form="profile-form" variant="primary" busy={busy} disabled={name === null}>
            Save
          </Button>
        </>
      }
    >
      <form id="profile-form" onSubmit={save}>
        <FormRow label="Name" htmlFor="profile-name">
          <Input id="profile-name" value={value} onChange={(e) => setName(e.target.value)} maxLength={100} />
        </FormRow>
        <FormRow label="Email" description="Ask a platform admin to change it.">
          <Input value={session?.user?.email ?? ""} readOnly disabled aria-label="Email" />
        </FormRow>
        <FormRow label="Platform role">
          <div>
            <Badge tone={session?.user?.platform_role === "platform_admin" ? "accent" : "muted"}>
              {session?.user?.platform_role === "platform_admin" ? "Platform admin" : "User"}
            </Badge>
          </div>
        </FormRow>
        {err && <Alert>{err}</Alert>}
      </form>
    </Panel>
  );
}

function PasswordCard() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      await api.changePassword({ current_password: current, new_password: next });
      setCurrent("");
      setNext("");
      setMsg({ ok: true, text: "Password changed. Your other sessions were signed out." });
    } catch (e) {
      setMsg({ ok: false, text: e instanceof ApiRequestError && e.code === "invalid_credentials" ? "The current password is wrong." : errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Password"
      description="Changing it signs out your other sessions."
      footer={
        <Button type="submit" form="password-form" variant="primary" busy={busy} disabled={next.length < 12 || !current}>
          Change password
        </Button>
      }
    >
      <form id="password-form" onSubmit={save}>
        <FormRow label="Current password" htmlFor="current-password">
          <Input id="current-password" type="password" required autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
        </FormRow>
        <FormRow label="New password" description="At least 12 characters." htmlFor="new-password">
          <Input id="new-password" type="password" required minLength={12} autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
        </FormRow>
        {msg && <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>}
      </form>
    </Panel>
  );
}

function RecoveryCard() {
  const q = useQuery({ queryKey: ["me", "recovery"], queryFn: api.recoveryCodes });
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const regenerate = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.reauth({ password, code });
      setCodes((await api.regenerateRecoveryCodes()).codes);
      setPassword("");
      setCode("");
      await qc.invalidateQueries({ queryKey: ["me", "recovery"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const left = q.data?.remaining ?? 0;
  return (
    <Panel title="Two-factor recovery codes" description="Each signs you in once if you lose your authenticator.">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-[13px]">
          <span className={left <= 3 ? "text-warn-text" : undefined}>{left} of 10</span> unused codes left.{" "}
          {left <= 3 && <span className="text-warn-text">Make new ones before you run out.</span>}
        </p>
        <Button onClick={() => setOpen(true)}>New codes</Button>
      </div>
      <Dialog
        title="New recovery codes"
        open={open}
        onOpenChange={(o) =>
          !o &&
          (() => {
            setOpen(false);
            setCodes(null);
          })()
        }
      >
        {codes ? (
          <div className="flex flex-col gap-3">
            <p className="text-sm text-muted">Your old codes no longer work. Save these; they are shown only once.</p>
            <ul className="grid grid-cols-2 gap-1 rounded-md border border-line bg-surface-2 p-3 font-mono text-sm" data-testid="new-recovery-codes">
              {codes.map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
            <Button
              variant="primary"
              onClick={() => {
                setOpen(false);
                setCodes(null);
              }}
            >
              Done
            </Button>
          </div>
        ) : (
          <form className="flex flex-col gap-3" onSubmit={regenerate}>
            <p className="text-sm text-muted">Replaces all your recovery codes. Confirm with your password and a code.</p>
            <Field label="Password">{(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
            <Field label="Authenticator code">{(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
            {err && <Alert>{err}</Alert>}
            <Button type="submit" variant="primary" busy={busy} disabled={!password || code.length < 6}>
              Make new codes
            </Button>
          </form>
        )}
      </Dialog>
    </Panel>
  );
}

function SessionsCard() {
  const q = useQuery({ queryKey: ["me", "sessions"], queryFn: api.mySessions });
  const qc = useQueryClient();
  const navigate = useNavigate();
  return (
    <>
      {q.data && (
        <Table head={["Device", "IP", "Signed in", "Last seen", ""]}>
          {q.data.items.map((s) => (
            <tr key={s.id}>
              <td className="max-w-xs truncate px-3 py-1.5 text-xs" title={s.user_agent ?? ""}>
                {s.user_agent ?? "—"} {s.current && <Badge tone="accent">this one</Badge>}
              </td>
              <td className="px-3 py-1.5 font-mono text-xs">{s.ip ?? "—"}</td>
              <td className="px-3 py-1.5 text-xs text-muted">{formatDate(s.created_at)}</td>
              <td className="px-3 py-1.5 text-xs text-muted">{relativeTime(s.last_seen_at)}</td>
              <td className="px-3 py-1.5 text-right">
                <Button
                  className="text-xs"
                  onClick={async () => {
                    await api.revokeSession(s.id);
                    if (s.current) {
                      qc.clear();
                      await navigate({ to: "/login" });
                    } else await qc.invalidateQueries({ queryKey: ["me", "sessions"] });
                  }}
                >
                  {s.current ? "Sign out" : "Revoke"}
                </Button>
              </td>
            </tr>
          ))}
        </Table>
      )}
    </>
  );
}

/** Two-factor sign-in is off until the user turns it on here; platform staff keep it on. */
function TwoFactorCard() {
  const q = useQuery(twoFactorQuery);
  const qc = useQueryClient();
  const [dialog, setDialog] = useState<"on" | "off" | null>(null);
  if (!q.data) return null;
  const { enabled, required } = q.data;
  const close = async () => {
    setDialog(null);
    await qc.invalidateQueries({ queryKey: ["me"] });
  };
  return (
    <>
      <Panel
        title="Two-factor sign-in"
        description="Ask for a code from an authenticator app (1Password, Aegis, Google Authenticator…) as well as your password."
      >
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-[13px]">
            {enabled ? <Badge tone="ok">On</Badge> : <Badge>Off</Badge>}{" "}
            {enabled
              ? required
                ? "Required for platform staff."
                : "Signing in needs your password and a code."
              : "Signing in needs only your password."}
          </p>
          {!enabled && (
            <Button variant="primary" onClick={() => setDialog("on")}>
              Turn on
            </Button>
          )}
          {enabled && !required && <Button onClick={() => setDialog("off")}>Turn off</Button>}
        </div>
      </Panel>
      {enabled && <RecoveryCard />}
      <TurnOnDialog open={dialog === "on"} onClose={close} />
      <TurnOffDialog open={dialog === "off"} onClose={close} />
    </>
  );
}

function TurnOnDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const [setup, setSetup] = useState<{ setup_token: string; totp_uri: string; totp_secret: string } | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const reset = () => {
    setPassword("");
    setSetup(null);
    setCodes(null);
    setErr(null);
  };
  const leave = () => {
    reset();
    onClose();
  };
  const begin = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      setSetup(await api.setupTotp({ password }));
      setPassword("");
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Turn on two-factor sign-in" open={open} onOpenChange={(o) => !o && leave()}>
      {codes ? (
        <RecoveryCodes codes={codes} onDone={leave} doneLabel="Done" />
      ) : setup ? (
        <TotpEnrol
          uri={setup.totp_uri}
          secret={setup.totp_secret}
          submitLabel="Verify and turn on"
          onCode={async (code) => setCodes((await api.enableTotp({ setup_token: setup.setup_token, code })).codes)}
        />
      ) : (
        <form className="flex flex-col gap-3" onSubmit={begin}>
          <p className="text-sm text-muted">Confirm it's you with your password. Next you'll scan a code with your authenticator app.</p>
          <Field label="Password">{(id) => <Input id={id} type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
          {err && <Alert>{err}</Alert>}
          <Button type="submit" variant="primary" busy={busy} disabled={!password}>
            Continue
          </Button>
        </form>
      )}
    </Dialog>
  );
}

function TurnOffDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.disableTotp({ password, code });
      setPassword("");
      setCode("");
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Turn off two-factor sign-in" open={open} onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-3" onSubmit={submit}>
        <p className="text-sm text-muted">Signing in will need only your password. Confirm with your password and a current code (or a recovery code).</p>
        <Field label="Password">{(id) => <Input id={id} type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
        <Field label="Authenticator or recovery code">{(id) => <Input id={id} autoComplete="one-time-code" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
        {err && <Alert>{err}</Alert>}
        <Button type="submit" variant="danger" busy={busy} disabled={!password || code.length < 6}>
          Turn off
        </Button>
      </form>
    </Dialog>
  );
}
