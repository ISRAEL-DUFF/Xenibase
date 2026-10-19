import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  ApiRequestError,
  api,
  errorMessage,
  setReauthHandler,
} from "../api/client";
import { Alert, Button, Dialog, Field, Input } from "./ui";
import { useTwoFactor } from "../lib/twofactor";

/** Runs an action, asking for the password and a code first when the
 * server wants a fresh step-up (spending money, V2 §7.2). */
export function useStepUp(
  why = "This spends money: confirm with your password and a code.",
) {
  const [pending, setPending] = useState<(() => Promise<unknown>) | null>(null);
  const [password, setPassword] = useState("");
  const twoFactor = useTwoFactor();
  const [code, setCode] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const run = async (f: () => Promise<unknown>) => {
    try {
      await f();
    } catch (e) {
      if (e instanceof ApiRequestError && e.code === "reauth_required") {
        setPending(() => f);
        return;
      }
      throw e;
    }
  };
  const confirm = async (e: FormEvent) => {
    e.preventDefault();
    if (!pending) return;
    setBusy(true);
    setErr(null);
    try {
      await api.reauth({ password, code: twoFactor ? code : undefined });
      await pending();
      setPending(null);
      setPassword("");
      setCode("");
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const dialog = (
    <Dialog
      open={!!pending}
      onOpenChange={(o) => !o && setPending(null)}
      title="Confirm it's you"
      description={why}
    >
      <form className="flex flex-col gap-3" onSubmit={confirm}>
        <Field label="Your password">
          {(id) => (
            <Input
              id={id}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        {twoFactor && (
<Field label="Your authenticator code">
          {(id) => (
            <Input
              id={id}
              inputMode="numeric"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          )}
        </Field>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={() => setPending(null)}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            busy={busy}
            disabled={!password || (twoFactor && code.length < 6)}
          >
            Confirm
          </Button>
        </div>
      </form>
    </Dialog>
  );
  return { run, dialog };
}

/**
 * Answers every request the server refuses with reauth_required (refunds,
 * credit notes, deleting a project…): asks for the password and a code,
 * then the request is sent again. Mounted once, at the root.
 */
export function StepUpHost() {
  const resolve = useRef<((ok: boolean) => void) | null>(null);
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");
  const twoFactor = useTwoFactor();
  const [code, setCode] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    setReauthHandler(
      () =>
        new Promise<boolean>((r) => {
          resolve.current?.(false);
          resolve.current = r;
          setPassword("");
          setCode("");
          setErr(null);
          setOpen(true);
        }),
    );
    return () => setReauthHandler(null);
  }, []);
  const finish = (ok: boolean) => {
    resolve.current?.(ok);
    resolve.current = null;
    setOpen(false);
  };
  const confirm = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.reauth({ password, code: twoFactor ? code : undefined });
      finish(true);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && finish(false)}
      title="Confirm it's you"
      description="This action needs your password and a code."
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={confirm}
        data-testid="step-up"
      >
        <Field label="Your password">
          {(id) => (
            <Input
              id={id}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        {twoFactor && (
<Field label="Your authenticator code">
          {(id) => (
            <Input
              id={id}
              inputMode="numeric"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          )}
        </Field>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={() => finish(false)}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            busy={busy}
            disabled={!password || (twoFactor && code.length < 6)}
          >
            Confirm
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
