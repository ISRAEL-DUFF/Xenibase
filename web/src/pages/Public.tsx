import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage, type LoginChallenge, type SessionState } from "../api/client";
import { RecoveryCodes, TotpEnrol } from "../components/Enrol";
import { AuthShell, TermsText } from "../components/Layout";
import { Alert, Button, Field, Input, Spinner } from "../components/ui";
import { setCurrentOrg } from "../lib/org";
import { sessionQuery, setSession } from "../lib/session";
import { Turnstile } from "../components/Turnstile";

function PasswordFields({ password, setPassword, confirm, setConfirm }: { password: string; setPassword: (v: string) => void; confirm: string; setConfirm: (v: string) => void }) {
  const mismatch = confirm !== "" && confirm !== password;
  return (
    <>
      <Field label="Password" hint="At least 12 characters.">
        {(id) => <Input id={id} type="password" required minLength={12} autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />}
      </Field>
      <Field label="Confirm password" error={mismatch ? "Passwords do not match." : null}>
        {(id) => <Input id={id} type="password" required autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />}
      </Field>
    </>
  );
}

function TermsAgreement({ checked, setChecked }: { checked: boolean; setChecked: (v: boolean) => void }) {
  const q = useQuery({ queryKey: ["terms"], queryFn: api.terms });
  const [open, setOpen] = useState(false);
  return (
    <div className="flex flex-col gap-2">
      <label className="flex items-start gap-2 text-sm">
        <input type="checkbox" checked={checked} onChange={(e) => setChecked(e.target.checked)} className="mt-1" data-testid="accept-terms" />
        <span>
          I accept the{" "}
          <button type="button" className="underline" onClick={() => setOpen((o) => !o)}>
            terms of use and privacy notice
          </button>
          {q.data ? ` (version ${q.data.version})` : ""}.
        </span>
      </label>
      {open && q.data && <TermsText terms={q.data} />}
    </div>
  );
}

/** Sign-up, in approval and open modes (V2 §3.1). */
export function SignupPage() {
  const { data: session } = useQuery(sessionQuery);
  const terms = useQuery({ queryKey: ["terms"], queryFn: api.terms });
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [accepted, setAccepted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [sent, setSent] = useState(false);
  const [challenge, setChallenge] = useState("");
  const siteKey = session?.turnstile_site_key;

  if (session?.signup_mode === "invite_only") {
    return (
      <AuthShell>
        <h1 className="mb-2 text-lg font-semibold">Sign-up is by invitation</h1>
        <p className="text-sm text-muted">Ask someone who uses this Xenibase to invite you. The email it sends has a link that creates your account.</p>
        <Link to="/login" className="mt-4 block text-sm underline">
          Back to sign in
        </Link>
      </AuthShell>
    );
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.signup({ email, password, name: name || undefined, terms_version: terms.data?.version ?? 0, challenge: challenge || undefined });
      setSent(true);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (sent) {
    return (
      <AuthShell>
        <h1 className="mb-2 text-lg font-semibold">Check your email</h1>
        <p className="text-sm text-muted">
          We sent a link to <strong>{email}</strong>. Open it to confirm your address
          {session?.signup_mode === "approval" ? "; then the platform admin approves your account" : ""}.
        </p>
      </AuthShell>
    );
  }

  return (
    <AuthShell>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <h1 className="text-lg font-semibold">Create an account</h1>
        <Field label="Name">{(id) => <Input id={id} value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" maxLength={100} />}</Field>
        <Field label="Email">{(id) => <Input id={id} type="email" required value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" />}</Field>
        <PasswordFields password={password} setPassword={setPassword} confirm={confirm} setConfirm={setConfirm} />
        <TermsAgreement checked={accepted} setChecked={setAccepted} />
        {siteKey && <Turnstile siteKey={siteKey} onToken={setChallenge} />}
        {err && <Alert>{err}</Alert>}
        <Button
          type="submit"
          variant="primary"
          busy={busy}
          disabled={!accepted || password.length < 12 || password !== confirm || (!!siteKey && !challenge)}
        >
          Create account
        </Button>
        <Link to="/login" className="text-xs text-muted hover:text-fg">
          Already have an account? Sign in
        </Link>
      </form>
    </AuthShell>
  );
}

/** The link in the verification email. */
export function VerifyEmailPage() {
  const { token } = useSearch({ from: "/verify-email" });
  const [state, setState] = useState<{ ok?: boolean; approved?: boolean; err?: string }>({});
  const started = useRef(false);
  // After the session query, which hands out the CSRF token this write needs.
  const { isSuccess: ready } = useQuery(sessionQuery);
  useEffect(() => {
    if (started.current || !token || !ready) return;
    started.current = true;
    api.verifyEmail(token).then(
      (r) => setState({ ok: true, approved: r.approved }),
      (e) => setState({ err: errorMessage(e) }),
    );
  }, [token, ready]);
  return (
    <AuthShell>
      {!token && <Alert>This link is missing its token.</Alert>}
      {token && !state.ok && !state.err && <Spinner />}
      {state.err && <Alert>{state.err}</Alert>}
      {state.ok && (
        <div className="flex flex-col gap-3" data-testid="email-verified">
          <h1 className="text-lg font-semibold">Email confirmed</h1>
          <p className="text-sm text-muted">
            {state.approved ? "Sign in to set up two-factor authentication." : "The platform admin will approve your account; you'll get an email when they do."}
          </p>
          {state.approved && (
            <Link to="/login" className="rounded-md bg-accent px-3 py-1.5 text-center text-sm font-medium text-accent-fg">
              Sign in
            </Link>
          )}
        </div>
      )}
    </AuthShell>
  );
}

/** Confirms a billing contact from the emailed link. It asks first, with a
 * button, so a mail scanner opening the link confirms nothing. */
export function ConfirmContactPage() {
  const { token } = useSearch({ from: "/confirm-contact" });
  const [busy, setBusy] = useState(false);
  const [state, setState] = useState<{ org?: string; err?: string }>({});
  // The session query hands out the CSRF token the write needs.
  const { isSuccess: ready } = useQuery(sessionQuery);
  const confirm = async () => {
    if (!token) return;
    setBusy(true);
    try {
      const r = await api.confirmBillingContact(token);
      setState({ org: r.org_name });
    } catch (e) {
      setState({ err: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <AuthShell>
      {!token && <Alert>This link is missing its token.</Alert>}
      {state.err && <Alert>{state.err}</Alert>}
      {token && state.org === undefined && (
        <div className="flex flex-col gap-3">
          <h1 className="text-lg font-semibold">Confirm billing contact</h1>
          <p className="text-sm text-muted">
            Once you confirm, this address gets the organisation&apos;s billing
            email, and email you send to Xenibase support opens tickets its
            owners and admins can read. Confirm only if you work with them.
          </p>
          <Button variant="primary" busy={busy} disabled={!ready} onClick={() => void confirm()} data-testid="confirm-contact">
            Confirm
          </Button>
        </div>
      )}
      {state.org !== undefined && (
        <div className="flex flex-col gap-3" data-testid="contact-confirmed">
          <h1 className="text-lg font-semibold">Confirmed</h1>
          <p className="text-sm text-muted">You&apos;re a billing contact for {state.org}.</p>
        </div>
      )}
    </AuthShell>
  );
}

/** Asks for a reset link, or (with ?token=) sets the new password. */
export function ResetPasswordPage() {
  const { token } = useSearch({ from: "/reset-password" });
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      if (token) await api.confirmPasswordReset(token, password);
      else await api.requestPasswordReset(email);
      setDone(true);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthShell>
      {done ? (
        <div className="flex flex-col gap-3">
          <h1 className="text-lg font-semibold">{token ? "Password changed" : "Check your email"}</h1>
          <p className="text-sm text-muted">
            {token ? "You've been signed out everywhere. Sign in with the new password." : "If the address has an account, a reset link is on its way. It works for one hour."}
          </p>
          <Link to="/login" className="text-sm underline">
            Sign in
          </Link>
        </div>
      ) : (
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <h1 className="text-lg font-semibold">{token ? "Choose a new password" : "Reset your password"}</h1>
          {token ? (
            <PasswordFields password={password} setPassword={setPassword} confirm={confirm} setConfirm={setConfirm} />
          ) : (
            <Field label="Email">{(id) => <Input id={id} type="email" required value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />}</Field>
          )}
          {err && <Alert>{err}</Alert>}
          <Button type="submit" variant="primary" busy={busy} disabled={!!token && (password.length < 12 || password !== confirm)}>
            {token ? "Set password" : "Send a reset link"}
          </Button>
        </form>
      )}
    </AuthShell>
  );
}

/** The link in an invitation email (V2 §3.2). */
export function InvitePage() {
  const { token } = useSearch({ from: "/invite" });
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { data: session } = useQuery(sessionQuery);
  // Waits for the session, which hands out the CSRF token the preview needs.
  const preview = useQuery({ queryKey: ["invitation", token], queryFn: () => api.previewInvitation(token ?? ""), enabled: !!token && !!session, retry: false });
  const terms = useQuery({ queryKey: ["terms"], queryFn: api.terms });
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [accepted, setAccepted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [login, setLogin] = useState<LoginChallenge | null>(null);
  const [signedIn, setSignedIn] = useState<SessionState | null>(null);

  const goTo = async (org?: string | null) => {
    if (org) setCurrentOrg(org);
    await qc.invalidateQueries();
    await navigate({ to: "/projects" });
  };

  const accept = async (e?: FormEvent) => {
    e?.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const r = await api.acceptInvitation(
        session?.authenticated ? { token: token ?? "" } : { token: token ?? "", name: name || undefined, password, terms_version: terms.data?.version },
      );
      if (r.login?.session) {
        // No two-factor sign-in on a new account: the invitation signed them in.
        setSession(qc, r.login.session);
        await goTo(r.org_id);
      } else if (r.login) {
        setLogin(r.login);
        if (r.org_id) setCurrentOrg(r.org_id);
      } else await goTo(r.org_id);
    } catch (e) {
      setErr(e instanceof ApiRequestError && e.code === "sign_in_to_accept" ? "You already have an account: sign in, then open this link again." : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (signedIn?.recovery_codes) {
    return (
      <AuthShell wide>
        <RecoveryCodes
          codes={signedIn.recovery_codes}
          onDone={async () => {
            setSession(qc, signedIn);
            await goTo();
          }}
        />
      </AuthShell>
    );
  }
  if (login?.enrollment) {
    return (
      <AuthShell wide>
        <TotpEnrol
          uri={login.enrollment.totp_uri}
          secret={login.enrollment.totp_secret}
          submitLabel="Verify and continue"
          onCode={async (c) => setSignedIn(await api.totp({ challenge_id: login.challenge_id ?? "", code: c }))}
        />
      </AuthShell>
    );
  }

  const p = preview.data;
  return (
    <AuthShell>
      {!token && <Alert>This link is missing its token.</Alert>}
      {preview.isPending && token && <Spinner />}
      {preview.isError && <Alert>This invitation is invalid, expired, or already used.</Alert>}
      {p && (
        <div className="flex flex-col gap-4">
          <h1 className="text-lg font-semibold">{p.org_name ? `Join ${p.org_name}` : "Join Xenibase"}</h1>
          <p className="text-sm text-muted">
            {p.invited_by} invited <strong>{p.email}</strong>
            {p.org_name ? ` as ${p.role}${p.project_count ? `, with access to ${p.project_count} project${p.project_count === 1 ? "" : "s"}` : ""}` : ""}.
          </p>
          {session?.authenticated ? (
            session.user?.email.toLowerCase() === p.email.toLowerCase() ? (
              <Button variant="primary" busy={busy} onClick={() => accept()}>
                Accept invitation
              </Button>
            ) : (
              <Alert tone="warn">
                You're signed in as {session.user?.email}, but this invitation is for {p.email}. Sign out and open the link again.
              </Alert>
            )
          ) : p.has_account ? (
            <Alert tone="accent">
              {p.email} already has an account.{" "}
              <Link to="/login" search={{ next: `/invite?token=${token}` }} className="underline">
                Sign in to accept
              </Link>
              .
            </Alert>
          ) : (
            <form className="flex flex-col gap-4" onSubmit={accept}>
              <Field label="Your name">{(id) => <Input id={id} value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" maxLength={100} />}</Field>
              <PasswordFields password={password} setPassword={setPassword} confirm={confirm} setConfirm={setConfirm} />
              <TermsAgreement checked={accepted} setChecked={setAccepted} />
              <Button type="submit" variant="primary" busy={busy} disabled={!accepted || password.length < 12 || password !== confirm}>
                Create account and join
              </Button>
            </form>
          )}
          {err && <Alert>{err}</Alert>}
        </div>
      )}
    </AuthShell>
  );
}
