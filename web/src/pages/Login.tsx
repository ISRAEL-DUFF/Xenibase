import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage, type LoginChallenge, type SessionState } from "../api/client";
import { RecoveryCodes, TotpEnrol } from "../components/Enrol";
import { AuthShell } from "../components/Layout";
import { Alert, Button, Field, Input } from "../components/ui";
import { sessionQuery, setSession } from "../lib/session";

export function LoginPage() {
  const { next } = useSearch({ from: "/login" });
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { data: session } = useQuery(sessionQuery);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [challenge, setChallenge] = useState<LoginChallenge | null>(null);
  const [signedIn, setSignedIn] = useState<SessionState | null>(null);
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<{ msg: string; code?: string } | null>(null);
  const [resent, setResent] = useState(false);

  const done = async (s: SessionState) => {
    setSession(qc, s);
    await navigate({ to: next ?? "/projects" });
  };

  const finish = async (s: SessionState) => {
    // This sign-in enrolled an authenticator (a platform admin's first): show the codes first.
    if (s.recovery_codes?.length) setSignedIn(s);
    else await done(s);
  };

  const submitPassword = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    setResent(false);
    try {
      const res = await api.login({ email, password });
      setPassword("");
      // An account without two-factor sign-in is signed in by its password alone.
      if (res.session) await done(res.session);
      else setChallenge(res);
    } catch (e) {
      setErr({ msg: errorMessage(e), code: e instanceof ApiRequestError ? e.code : undefined });
    } finally {
      setBusy(false);
    }
  };

  const submitCode = async (e: FormEvent) => {
    e.preventDefault();
    if (!challenge) return;
    setBusy(true);
    setErr(null);
    try {
      await finish(await api.totp({ challenge_id: challenge.challenge_id ?? "", code }));
    } catch (e) {
      setErr({ msg: errorMessage(e) });
      setCode("");
    } finally {
      setBusy(false);
    }
  };

  if (signedIn?.recovery_codes) {
    return (
      <AuthShell wide>
        <RecoveryCodes codes={signedIn.recovery_codes} onDone={() => done(signedIn)} />
      </AuthShell>
    );
  }

  if (challenge?.enrollment) {
    const enr = challenge.enrollment;
    return (
      <AuthShell wide>
        <TotpEnrol
          uri={enr.totp_uri}
          secret={enr.totp_secret}
          submitLabel="Verify and sign in"
          onCode={async (c) => finish(await api.totp({ challenge_id: challenge.challenge_id ?? "", code: c }))}
        />
      </AuthShell>
    );
  }

  return (
    <AuthShell>
      {!challenge ? (
        <form className="flex flex-col gap-4" onSubmit={submitPassword}>
          <h1 className="text-lg font-semibold">Sign in</h1>
          <Field label="Email">
            {(id) => <Input id={id} type="email" autoComplete="username" required value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />}
          </Field>
          <Field label="Password">
            {(id) => (
              <Input id={id} type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
            )}
          </Field>
          {err && (
            <Alert>
              {err.msg}
              {err.code === "email_unverified" && (
                <div className="mt-2">
                  <Button
                    className="text-xs"
                    onClick={async () => {
                      await api.resendVerification(email).catch(() => {});
                      setResent(true);
                    }}
                  >
                    Send the link again
                  </Button>
                </div>
              )}
            </Alert>
          )}
          {resent && <Alert tone="ok">If the address has an account waiting for verification, a new link is on its way.</Alert>}
          <Button type="submit" variant="primary" busy={busy}>
            Continue
          </Button>
          <div className="flex justify-between text-[13px]">
            <Link to="/reset-password" className="text-muted hover:text-accent-text hover:underline">
              Forgot your password?
            </Link>
            {session && session.signup_mode !== "invite_only" && (
              <Link to="/signup" className="text-accent-text hover:underline">
                Create an account
              </Link>
            )}
          </div>
        </form>
      ) : (
        <form className="flex flex-col gap-4" onSubmit={submitCode}>
          <h1 className="text-lg font-semibold">Two-factor code</h1>
          <p className="text-sm text-muted">Enter the 6-digit code from your authenticator app, or one of your recovery codes.</p>
          <Field label="Code">
            {(id) => (
              <Input
                id={id}
                autoComplete="one-time-code"
                required
                value={code}
                onChange={(e) => setCode(e.target.value)}
                autoFocus
                className="font-mono tracking-widest"
              />
            )}
          </Field>
          {err && <Alert>{err.msg}</Alert>}
          <Button type="submit" variant="primary" busy={busy}>
            Sign in
          </Button>
          <Button variant="ghost" onClick={() => setChallenge(null)}>
            Start over
          </Button>
        </form>
      )}
    </AuthShell>
  );
}
