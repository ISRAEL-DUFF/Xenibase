import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage, type BackupKeyInfo, type StorageRequest, type StorageTestResult } from "../api/client";
import { formatBytes, formatDate, relativeTime } from "../lib/format";
import { Alert, Badge, Button, CodeBlock, CopyField, Field, Input, Dialog, StatusBadge, Table, PanelSkeleton } from "./ui";
import { useTwoFactor } from "../lib/twofactor";

// ---- S3 storage ------------------------------------------------------------

/** S3 settings with the live write/read/list/delete test (spec §8.2 step 3). */
export function StorageForm({ onSaved, submitLabel = "Test and save" }: { onSaved?: () => void; submitLabel?: string }) {
  const qc = useQueryClient();
  const current = useQuery({ queryKey: ["settings", "storage"], queryFn: api.storage });
  const [form, setForm] = useState<StorageRequest | null>(null);
  const [result, setResult] = useState<StorageTestResult | null>(null);
  const [busy, setBusy] = useState<"test" | "save" | null>(null);
  const [err, setErr] = useState<string | null>(null);

  if (current.isPending) return <PanelSkeleton />;
  const c = current.data;
  const v: StorageRequest = form ?? {
    endpoint: c?.endpoint ?? "",
    region: c?.region ?? "",
    bucket: c?.bucket ?? "",
    prefix: c?.prefix ?? "xenibase",
    access_key: c?.access_key ?? "",
    secret_key: "",
    path_style: c?.path_style ?? false,
  };
  const set = (patch: Partial<StorageRequest>) => {
    setForm({ ...v, ...patch });
    setResult(null);
  };
  const run = async (kind: "test" | "save") => {
    setBusy(kind);
    setErr(null);
    try {
      const r = kind === "test" ? await api.testStorage(v) : await api.saveStorage(v);
      setResult(r);
      if (r.saved) {
        await qc.invalidateQueries({ queryKey: ["settings", "storage"] });
        await qc.invalidateQueries({ queryKey: ["backups"] });
        setForm(null);
        onSaved?.();
      }
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void run("save");
  };

  return (
    <form className="flex flex-col gap-3" onSubmit={submit}>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Endpoint" hint="AWS, Backblaze B2, Cloudflare R2, MinIO, …">
          {(id) => (
            <Input
              id={id}
              required
              value={v.endpoint}
              onChange={(e) => set({ endpoint: e.target.value })}
              className="font-mono"
              placeholder="https://s3.eu-central-1.amazonaws.com"
            />
          )}
        </Field>
        <Field label="Region">
          {(id) => <Input id={id} value={v.region ?? ""} onChange={(e) => set({ region: e.target.value })} placeholder="us-east-1" />}
        </Field>
        <Field label="Bucket">
          {(id) => <Input id={id} required value={v.bucket} onChange={(e) => set({ bucket: e.target.value })} className="font-mono" />}
        </Field>
        <Field label="Prefix" hint="Objects go under this path in the bucket.">
          {(id) => <Input id={id} value={v.prefix ?? ""} onChange={(e) => set({ prefix: e.target.value })} className="font-mono" />}
        </Field>
        <Field label="Access key">
          {(id) => (
            <Input id={id} required value={v.access_key} onChange={(e) => set({ access_key: e.target.value })} className="font-mono" autoComplete="off" />
          )}
        </Field>
        <Field label="Secret key" hint={c?.configured ? "Leave empty to keep the stored one." : undefined}>
          {(id) => (
            <Input
              id={id}
              type="password"
              required={!c?.configured}
              value={v.secret_key ?? ""}
              onChange={(e) => set({ secret_key: e.target.value })}
              className="font-mono"
              autoComplete="off"
              placeholder={c?.configured ? "unchanged" : undefined}
            />
          )}
        </Field>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!v.path_style} onChange={(e) => set({ path_style: e.target.checked })} />
        Path-style addressing (MinIO and most self-hosted S3)
      </label>
      {result && (
        <Alert
          tone={result.ok ? "ok" : "danger"}
          title={result.ok ? (result.saved ? "Live test passed; saved" : "Live test passed") : "Live test failed; not saved"}
        >
          <ul className="text-xs" data-testid="storage-test-steps">
            {result.steps.map((s) => (
              <li key={s.step}>
                {s.ok ? "✓" : "✗"} {s.step} ({s.took_ms} ms){s.error && <span className="ml-1 break-all">: {s.error}</span>}
              </li>
            ))}
          </ul>
        </Alert>
      )}
      {err && <Alert>{err}</Alert>}
      <div className="flex gap-2">
        <Button onClick={() => run("test")} busy={busy === "test"} disabled={!v.endpoint || !v.bucket || !v.access_key}>
          Test only
        </Button>
        <Button type="submit" variant="primary" busy={busy === "save"}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}

// ---- Backup key --------------------------------------------------------------

function downloadKey(key: string, fingerprint: string) {
  const text = `# Xenibase backup encryption key (fingerprint ${fingerprint}).
# Without it, Xenibase's backups cannot be decrypted. Store it offline,
# away from this server (a password manager, printed in a safe).
${key}
`;
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  a.download = `xenibase-backup-key-${fingerprint}.txt`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 10_000);
}

/** Pulls the key line out of a pasted or uploaded key file. */
export function extractKey(text: string): string {
  const line = text.split(/\r?\n/).find((l) => l.trim().startsWith("xenibase-backup-key-v1:"));
  return (line ?? text).trim();
}

/**
 * Generate, download, and confirm the backup key (spec §8.2 step 4): the
 * operator must download it and prove they kept it by pasting it back.
 */
export function BackupKeyPanel({ onConfirmed }: { onConfirmed?: () => void }) {
  const qc = useQueryClient();
  const info = useQuery({ queryKey: ["settings", "backup-key"], queryFn: api.backupKey });
  const [key, setKey] = useState<string | null>(null);
  const [downloaded, setDownloaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [reauth, setReauth] = useState(false);
  const [reconfirm, setReconfirm] = useState(false);

  if (info.isPending) return <PanelSkeleton rows={2} />;
  const k = info.data;

  const generate = async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await api.generateBackupKey();
      setKey(r.key);
      qc.setQueryData(["settings", "backup-key"], r.info);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (!k?.exists) {
    return (
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted">
          Backups are encrypted before upload with a key Xenibase generates. Losing it means losing the backups, so you download it now and keep it offline.
        </p>
        {err && <Alert>{err}</Alert>}
        <div>
          <Button variant="primary" onClick={generate} busy={busy}>
            Generate backup key
          </Button>
        </div>
      </div>
    );
  }

  if (key || !k.confirmed_at || reconfirm) {
    return (
      <div className="flex flex-col gap-3">
        {key && (
          <>
            <Alert tone="warn" title="Download the key and store it offline">
              It decrypts every backup. Anyone with it and your bucket can read your data; without it, nobody can, including you.
            </Alert>
            <CopyField label="Backup key" value={key} secret testId="backup-key" />
            <div>
              <Button
                variant="primary"
                onClick={() => {
                  downloadKey(key, k.fingerprint ?? "key");
                  setDownloaded(true);
                }}
              >
                Download key file
              </Button>
            </div>
          </>
        )}
        {!key && !k.confirmed_at && (
          <Alert tone="warn" title="The backup key was never confirmed">
            Paste it back (or load the file) to prove it is stored safely, or download it again.
          </Alert>
        )}
        <ConfirmKeyForm
          disabled={!!key && !downloaded}
          onConfirmed={(i) => {
            qc.setQueryData(["settings", "backup-key"], i);
            setKey(null);
            setReconfirm(false);
            onConfirmed?.();
          }}
        />
        {!key && (
          <div>
            <Button variant="ghost" className="px-0 text-xs" onClick={() => setReauth(true)}>
              Download the key again…
            </Button>
          </div>
        )}
        <ExportKeyModal open={reauth} onClose={() => setReauth(false)} info={k} />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted">Fingerprint</dt>
        <dd className="font-mono" data-testid="backup-key-fingerprint">
          {k.fingerprint}
        </dd>
        <dt className="text-muted">Created</dt>
        <dd>{formatDate(k.created_at)}</dd>
        <dt className="text-muted">Last confirmed</dt>
        <dd>
          {formatDate(k.confirmed_at)} ({relativeTime(k.confirmed_at)})
        </dd>
      </dl>
      <div className="flex gap-2">
        <Button className="text-xs" onClick={() => setReconfirm(true)}>
          Re-confirm
        </Button>
        <Button className="text-xs" onClick={() => setReauth(true)}>
          Download again…
        </Button>
      </div>
      <ExportKeyModal open={reauth} onClose={() => setReauth(false)} info={k} />
      {onConfirmed && (
        <div>
          <Button variant="primary" onClick={onConfirmed}>
            Continue
          </Button>
        </div>
      )}
    </div>
  );
}

function ConfirmKeyForm({ disabled, onConfirmed }: { disabled: boolean; onConfirmed: (i: BackupKeyInfo) => void }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      onConfirmed(await api.confirmBackupKey(extractKey(text)));
      setText("");
    } catch (e) {
      setErr(e instanceof ApiRequestError && e.status === 400 ? "That is not this installation's backup key." : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="flex flex-col gap-2" onSubmit={submit}>
      <Field label="Confirm: paste the key or load the downloaded file">
        {(id) => (
          <textarea
            id={id}
            rows={2}
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              setErr(null);
            }}
            disabled={disabled}
            className="w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-xs text-fg focus:border-accent focus:outline-none disabled:opacity-50"
            placeholder="xenibase-backup-key-v1:…"
            data-testid="confirm-backup-key"
          />
        )}
      </Field>
      <input
        type="file"
        accept=".txt,text/plain"
        disabled={disabled}
        aria-label="Load key file"
        className="text-xs"
        onChange={async (e) => {
          const f = e.target.files?.[0];
          if (f) {
            setText(extractKey(await f.text()));
            setErr(null);
          }
        }}
      />
      {disabled && <p className="text-xs text-muted">Download the key file first.</p>}
      {err && <Alert>{err}</Alert>}
      <div>
        <Button type="submit" variant="primary" busy={busy} disabled={disabled || !text.trim()}>
          Confirm backup key
        </Button>
      </div>
    </form>
  );
}

function ExportKeyModal({ open, onClose, info }: { open: boolean; onClose: () => void; info: BackupKeyInfo }) {
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
      await api.reauth({ password, code: twoFactor ? code : undefined });
      const r = await api.exportBackupKey();
      downloadKey(r.key, info.fingerprint ?? "key");
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
    <Dialog title="Download the backup key" open={open} onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <p className="text-sm text-muted">Confirm it's you first.</p>
        <Field label="Your password">
          {(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />}
        </Field>
        {twoFactor && (
<Field label="Authenticator code">
          {(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" />}
        </Field>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" busy={busy} disabled={!password || (twoFactor && code.length < 6)}>
            Download
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// ---- Nodes ---------------------------------------------------------------------

/** Nodes and their agents (spec §8.1 /nodes), with registration tokens. */
export function NodesPanel() {
  const q = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, refetchInterval: 5000 });
  const [token, setToken] = useState<{ node: string; command: string; expires: string } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  if (q.isPending) return <PanelSkeleton rows={2} />;
  if (q.isError) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="flex flex-col gap-3">
      <Table head={["Node", "Address", "Agent", "Version", "Disk free", ""]}>
        {q.data.items.map((n) => {
          const m = n.agent.metrics as { disk_free_bytes?: number; disk_total_bytes?: number } | undefined;
          return (
            <tr key={n.id} data-testid="node-row">
              <td className="px-3 py-2 font-medium">
                {n.name} <Badge>{n.role}</Badge>
              </td>
              <td className="px-3 py-2 font-mono text-xs text-muted">{n.agent.address ?? n.private_addr}</td>
              <td className="px-3 py-2" data-testid="agent-status">
                {!n.agent.registered ? (
                  <Badge tone="warn">not registered</Badge>
                ) : n.agent.reachable ? (
                  <StatusBadge status="healthy" />
                ) : (
                  <span title={n.agent.error}>
                    <Badge tone="danger">unreachable</Badge>
                  </span>
                )}
              </td>
              <td className="px-3 py-2 text-xs text-muted">{n.agent.version ?? "—"}</td>
              <td className="px-3 py-2 text-xs text-muted">
                {m?.disk_free_bytes != null && m.disk_total_bytes ? `${formatBytes(m.disk_free_bytes)} of ${formatBytes(m.disk_total_bytes)}` : "—"}
              </td>
              <td className="px-3 py-2 text-right">
                <Button
                  className="text-xs"
                  onClick={async () => {
                    setErr(null);
                    try {
                      const t = await api.nodeToken(n.id);
                      setToken({ node: n.name, command: t.command, expires: t.expires_at });
                    } catch (e) {
                      setErr(errorMessage(e));
                    }
                  }}
                >
                  {n.agent.registered ? "Re-register…" : "Register agent…"}
                </Button>
              </td>
            </tr>
          );
        })}
      </Table>
      {err && <Alert>{err}</Alert>}
      {token && (
        <Alert tone="accent" title={`Register the agent on ${token.node}`}>
          <p className="mb-2 text-xs">Run this on the node within 24 hours (until {formatDate(token.expires)}). The token works once.</p>
          <CodeBlock code={token.command} />
        </Alert>
      )}
    </div>
  );
}
