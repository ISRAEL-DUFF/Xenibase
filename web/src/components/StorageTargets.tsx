import { useQuery, useQueryClient } from "@tanstack/react-query";
import { HardDrive } from "lucide-react";
import { useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage, type Project, type ProjectBackupStorage, type StorageTarget, type StorageTestResult } from "../api/client";
import type { components } from "../api/schema";
import { formatBytes, formatDate } from "../lib/format";
import { useOperationToast } from "./Toasts";
import { Alert, Badge, Button, Panel, EmptyState, Field, Input, Dialog, Select, Table, TableSkeleton, PanelSkeleton } from "./ui";
import { useTwoFactor } from "../lib/twofactor";

type TargetRequest = components["schemas"]["StorageTargetRequest"];

function TestSteps({ result }: { result: StorageTestResult }) {
  return (
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
  );
}

/**
 * Add or edit a storage target (V2 §6). Saving runs the live write, read,
 * list and delete test under the prefix and stores nothing unless it
 * passes; credentials are never shown again (empty keeps the stored ones).
 */
function TargetForm({ org, target, onDone }: { org?: string; target?: StorageTarget; onDone: () => void }) {
  const qc = useQueryClient();
  const [v, setV] = useState<TargetRequest>({
    name: target?.name ?? "",
    endpoint: target?.endpoint ?? "",
    region: target?.region ?? "",
    bucket: target?.bucket ?? "",
    prefix: target?.prefix ?? "xenibase",
    access_key: "",
    secret_key: "",
    path_style: target?.path_style ?? false,
    is_default: target?.is_default ?? false,
  });
  const [result, setResult] = useState<StorageTestResult | null>(null);
  const [busy, setBusy] = useState<"test" | "save" | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const set = (patch: Partial<TargetRequest>) => {
    setV({ ...v, ...patch });
    setResult(null);
  };
  const test = async () => {
    setBusy("test");
    setErr(null);
    try {
      setResult(await api.testStorageTarget(org, { ...v, target_id: target?.id }));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy("save");
    setErr(null);
    try {
      const r = target ? await api.updateStorageTarget(org, target.id, v) : await api.createStorageTarget(org, v);
      setResult(r.test);
      if (r.saved) {
        await qc.invalidateQueries({ queryKey: ["storage-targets"] });
        onDone();
      }
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  return (
    <form className="flex flex-col gap-3" onSubmit={save}>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Name">{(id) => <Input id={id} required maxLength={100} value={v.name} onChange={(e) => set({ name: e.target.value })} />}</Field>
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
        <Field label="Access key" hint={target ? "Leave empty to keep the stored one." : undefined}>
          {(id) => (
            <Input
              id={id}
              required={!target}
              value={v.access_key ?? ""}
              onChange={(e) => set({ access_key: e.target.value })}
              className="font-mono"
              autoComplete="off"
              placeholder={target ? "unchanged" : undefined}
            />
          )}
        </Field>
        <Field label="Secret key" hint={target ? "Leave empty to keep the stored one." : "Sealed with the master key and never shown again."}>
          {(id) => (
            <Input
              id={id}
              type="password"
              required={!target}
              value={v.secret_key ?? ""}
              onChange={(e) => set({ secret_key: e.target.value })}
              className="font-mono"
              autoComplete="off"
              placeholder={target ? "unchanged" : undefined}
            />
          )}
        </Field>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!v.path_style} onChange={(e) => set({ path_style: e.target.checked })} />
        Path-style addressing (MinIO and most self-hosted S3)
      </label>
      {!org && (
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={!!v.is_default} disabled={target?.is_default} onChange={(e) => set({ is_default: e.target.checked })} />
          The platform default (projects that haven't chosen a target back up here)
        </label>
      )}
      {result && <TestSteps result={result} />}
      {err && <Alert>{err}</Alert>}
      <div className="flex justify-end gap-2">
        <Button onClick={onDone}>Cancel</Button>
        <Button onClick={test} busy={busy === "test"} disabled={!v.endpoint || !v.bucket}>
          Test only
        </Button>
        <Button type="submit" variant="primary" busy={busy === "save"}>
          Test and save
        </Button>
      </div>
    </form>
  );
}

/** Deletes a target, asking again when its backups would become unrestorable. */
function DeleteTarget({ org, target, onClose }: { org?: string; target: StorageTarget; onClose: () => void }) {
  const qc = useQueryClient();
  const [accept, setAccept] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const holdsBackups = target.usage.backups > 0;
  const run = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.deleteStorageTarget(org, target.id, accept);
      await qc.invalidateQueries({ queryKey: ["storage-targets"] });
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title={`Delete ${target.name}`} open onOpenChange={(o) => !o && onClose()}>
      <div className="flex flex-col gap-3 text-sm">
        {target.usage.projects > 0 ? (
          <Alert tone="warn">{target.usage.projects} project(s) back up here. Switch them to another target (Backups → Storage) before deleting it.</Alert>
        ) : (
          <p className="text-muted">Xenibase forgets the target and its credentials. The bucket itself is not touched.</p>
        )}
        {holdsBackups && (
          <label className="flex items-start gap-2">
            <input type="checkbox" checked={accept} onChange={(e) => setAccept(e.target.checked)} data-testid="accept-unrestorable" />
            <span>
              It holds {target.usage.backups} unexpired backup(s) ({formatBytes(target.usage.bytes)}). I understand they become unrestorable from Xenibase.
            </span>
          </label>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" busy={busy} disabled={target.usage.projects > 0 || (holdsBackups && !accept)} onClick={run}>
            Delete target
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

/**
 * Storage targets: an organisation's own buckets (org set; owners and
 * admins), or the platform's (platform admin). Backups on platform targets
 * count against each organisation's backup quota; on an org's own target
 * they don't.
 */
export function StorageTargetsPanel({ org }: { org?: string }) {
  const q = useQuery({ queryKey: ["storage-targets", org ?? "platform"], queryFn: () => api.storageTargets(org) });
  const [editing, setEditing] = useState<StorageTarget | "new" | null>(null);
  const [deleting, setDeleting] = useState<StorageTarget | null>(null);
  return (
    <div className="flex flex-col gap-3" data-testid={org ? "org-storage-targets" : "platform-storage-targets"}>
      <p className="text-sm text-muted">
        {org
          ? "Buckets your organisation brings: any project in it can send its backups here (Backups → Storage). Storage here doesn't count against your backup quota, and only your organisation sees these targets."
          : "Available to every organisation. Exactly one is the default, which every project uses unless told otherwise. Storage here counts against each organisation's backup quota."}
      </p>
      {q.isPending && <TableSkeleton rows={2} />}
      {q.isError && <Alert>{errorMessage(q.error)}</Alert>}
      {q.data && q.data.items.length === 0 && editing !== "new" && <EmptyState title="No targets yet" icon={<HardDrive />} />}
      {q.data && q.data.items.length > 0 && (
        <Table head={["Name", "Bucket", "Used by", "Stored", ""]}>
          {q.data.items.map((t) => (
            <tr key={t.id} data-testid="storage-target-row">
              <td className="px-3 py-2 font-medium">
                {t.name} {t.is_default && <Badge tone="accent">default</Badge>}
                <div className="text-xs text-muted">added {formatDate(t.created_at)}</div>
              </td>
              <td className="px-3 py-2 font-mono text-xs text-muted">
                {t.endpoint}
                <br />
                {t.bucket}/{t.prefix}
              </td>
              <td className="px-3 py-2 text-muted">{t.usage.projects} project(s)</td>
              <td className="px-3 py-2 text-muted">
                {t.usage.backups} backup(s), {formatBytes(t.usage.bytes)}
              </td>
              <td className="px-3 py-2 text-right">
                <div className="flex justify-end gap-2">
                  <Button className="text-xs" onClick={() => setEditing(t)}>
                    Edit
                  </Button>
                  {!t.is_default && (
                    <Button className="text-xs" variant="ghost" onClick={() => setDeleting(t)}>
                      Delete
                    </Button>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </Table>
      )}
      {editing === null ? (
        <div>
          <Button onClick={() => setEditing("new")}>Add a target</Button>
        </div>
      ) : (
        <Panel title={editing === "new" ? "New storage target" : `Edit ${editing.name}`}>
          <TargetForm key={editing === "new" ? "new" : editing.id} org={org} target={editing === "new" ? undefined : editing} onDone={() => setEditing(null)} />
        </Panel>
      )}
      {deleting && <DeleteTarget org={org} target={deleting} onClose={() => setDeleting(null)} />}
    </div>
  );
}

function downloadText(text: string, filename: string) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 10_000);
}

/** Step-up auth, then the project's key file (README + OpenPGP key). */
function DownloadKeyModal({ p, onClose }: { p: Project; onClose: () => void }) {
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
      const text = await api.downloadProjectBackupKey(p.id);
      downloadText(text, `xenibase-backup-key-${p.id}.asc`);
      onClose();
    } catch (e) {
      setErr(e instanceof ApiRequestError && e.code === "invalid_credentials" ? "Wrong password or code." : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Download the project's backup key" open onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <p className="text-sm text-muted">
          The file holds the key and a README on decrypting and restoring with standard tools (gpg and pg_restore), without Xenibase. Downloads are recorded in
          the audit log. Confirm it's you first.
        </p>
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

const choiceValue = (id: string | null | undefined) => id ?? "default";

/**
 * Project → Backups → Storage (V2 §6): the target new backups go to, an
 * optional copy of existing ones, and the project's own backup key.
 */
export function ProjectStorageCard({ p, canManage }: { p: Project; canManage: boolean }) {
  const qc = useQueryClient();
  const toast = useOperationToast();
  const q = useQuery({ queryKey: ["project-backup-storage", p.id], queryFn: () => api.projectBackupStorage(p.id) });
  const [choice, setChoice] = useState<string | null>(null);
  const [copy, setCopy] = useState(false);
  const [deleteOriginals, setDeleteOriginals] = useState(false);
  const [busy, setBusy] = useState<"switch" | "key" | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [downloading, setDownloading] = useState(false);
  if (q.isPending) return <PanelSkeleton rows={2} />;
  if (q.isError) return <Alert>{errorMessage(q.error)}</Alert>;
  const st: ProjectBackupStorage = q.data;
  const current = choiceValue(st.target.id);
  const selected = choice ?? current;
  const switchTarget = async () => {
    setBusy("switch");
    setErr(null);
    try {
      const r = await api.setProjectStorageTarget(p.id, {
        target_id: selected === "default" ? null : selected,
        copy_existing: copy,
        delete_originals: copy && deleteOriginals,
      });
      if (r.operation) toast(r.operation.id, `Storage switch · ${p.name}`);
      setChoice(null);
      setCopy(false);
      setDeleteOriginals(false);
      await qc.invalidateQueries({ queryKey: ["project-backup-storage", p.id] });
      await qc.invalidateQueries({ queryKey: ["backups"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const enableKey = async (rotate: boolean) => {
    setBusy("key");
    setErr(null);
    try {
      await api.enableProjectBackupKey(p.id, rotate);
      await qc.invalidateQueries({ queryKey: ["project-backup-storage", p.id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  return (
    <Panel title="Storage">
      <div className="flex flex-col gap-4 text-sm" data-testid="project-storage">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2">
          <dt className="text-muted">New backups go to</dt>
          <dd data-testid="project-storage-target">
            {st.target.name} <Badge tone={st.target.kind === "org" ? "accent" : "muted"}>{st.target.kind === "org" ? "your organisation's" : "platform"}</Badge>
            <span className="ml-2 text-xs text-muted">
              {st.counts_toward_quota ? "counts toward your backup quota" : "doesn't count toward your backup quota"}
            </span>
          </dd>
          <dt className="text-muted">Encryption key</dt>
          <dd data-testid="project-backup-key">
            {st.key.enabled ? (
              <>
                this project's own key <span className="font-mono text-xs text-muted">{st.key.fingerprint}</span>
              </>
            ) : (
              "the instance backup key"
            )}
          </dd>
        </dl>
        {canManage && (
          <>
            <div className="flex flex-col gap-2 border-t border-line pt-3">
              <Field label="Send new backups to">
                {(id) => (
                  <Select id={id} value={selected} onChange={(e) => setChoice(e.target.value)} data-testid="storage-choice">
                    {st.choices.map((c) => (
                      <option key={choiceValue(c.id)} value={choiceValue(c.id)}>
                        {c.name}
                        {c.kind === "org" ? " (organisation)" : ""}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              {selected !== current && (
                <>
                  <p className="text-xs text-muted">
                    Existing backups stay where they are and remain restorable.
                    {p.tier === "dedicated" && " WAL-G is reconfigured (the instance restarts briefly) and a fresh base backup is taken at once."}
                  </p>
                  <label className="flex items-center gap-2">
                    <input type="checkbox" checked={copy} onChange={(e) => setCopy(e.target.checked)} />
                    Also copy existing backups there (verified by checksum)
                  </label>
                  {copy && (
                    <label className="flex items-center gap-2">
                      <input type="checkbox" checked={deleteOriginals} onChange={(e) => setDeleteOriginals(e.target.checked)} />
                      Delete the originals once copied
                    </label>
                  )}
                  <div>
                    <Button variant="primary" busy={busy === "switch"} onClick={switchTarget} disabled={p.status !== "active"}>
                      Switch storage
                    </Button>
                  </div>
                </>
              )}
            </div>
            <div className="flex flex-col gap-2 border-t border-line pt-3">
              <p className="text-xs text-muted">
                {st.key.enabled
                  ? "New backups are OpenPGP messages to this key; anyone holding the downloaded key file can restore them with gpg and pg_restore."
                  : "Give the project its own key so its backups can be restored with standard tools from just the downloaded key file. Existing backups keep their key."}
              </p>
              <div className="flex flex-wrap gap-2">
                {!st.key.enabled ? (
                  <Button busy={busy === "key"} onClick={() => enableKey(false)}>
                    Use a project key
                  </Button>
                ) : (
                  <>
                    <Button onClick={() => setDownloading(true)}>Download key…</Button>
                    <Button variant="ghost" busy={busy === "key"} onClick={() => enableKey(true)}>
                      Rotate key
                    </Button>
                  </>
                )}
              </div>
            </div>
          </>
        )}
        {err && <Alert>{err}</Alert>}
      </div>
      {downloading && <DownloadKeyModal p={p} onClose={() => setDownloading(false)} />}
    </Panel>
  );
}
