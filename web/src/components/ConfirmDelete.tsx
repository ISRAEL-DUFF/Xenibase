import { useState, type FormEvent, type ReactNode } from "react";
import { ApiRequestError, api, errorMessage } from "../api/client";
import { Alert, Button, Field, Input, Dialog } from "./ui";
import { useTwoFactor } from "../lib/twofactor";

/**
 * Typed-confirmation modal for destructive actions (spec §8.9). It always
 * asks for password + code up front, since the server requires a recent
 * step-up authentication (spec §7.2).
 */
export function ConfirmDestroy({
  open,
  onClose,
  title,
  name,
  description,
  action,
  run,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  name: string;
  description: string;
  action: string;
  run: () => Promise<void>;
  /** Extra options shown above the confirmation fields. */
  children?: ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const [password, setPassword] = useState("");
  const twoFactor = useTwoFactor();
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      // Skip a fresh step-up while the session is still inside its reauth
      // window; the server enforces the window either way.
      const s = await api.session();
      if (!s.reauth_until || new Date(s.reauth_until).getTime() <= Date.now() + 5_000) {
        await api.reauth({ password, code: twoFactor ? code : undefined });
      }
      await run();
      setTyped("");
      setPassword("");
      setCode("");
      onClose();
    } catch (e) {
      setErr(e instanceof ApiRequestError && e.code === "invalid_credentials" ? "Wrong password or code." : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog title={title} open={open} onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <p className="text-sm text-muted">{description}</p>
        {children}
        <Field label={`Type ${name} to confirm`}>
          {(id) => <Input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" data-testid="confirm-name" />}
        </Field>
        <Field label="Your password">
          {(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />}
        </Field>
        {twoFactor && (
<Field label="Authenticator code">
          {(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" data-testid="confirm-code" />}
        </Field>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="danger" busy={busy} disabled={typed !== name || !password || (twoFactor && code.length < 6)}>
            {action}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
