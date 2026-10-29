import { Link } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, errorMessage, type Project, type SwitchedCredentials } from "../api/client";
import { formatBytes, formatDate, relativeTime } from "../lib/format";
import { useOperationStream } from "../lib/useOperationStream";
import { OperationLog } from "./OperationLog";
import { Alert, Badge, Button, Panel, CopyField, cx, Field, Input, Table } from "./ui";

const STATE_TEXT: Record<string, { tone: "warn" | "danger"; title: string; body: string }> = {
  warn: {
    tone: "warn",
    title: "Nearly full",
    body: "This project is over 90% of its storage limit. At 100% it becomes read-only; at 120% apps can no longer connect.",
  },
  soft: {
    tone: "danger",
    title: "Read-only: storage limit reached",
    body: "New transactions are read-only by default. You can still delete data (in a read-write transaction, or from the SQL console), then reclaim the space.",
  },
  hard: {
    tone: "danger",
    title: "Offline: storage limit exceeded",
    body: "Apps can no longer connect. The SQL console and table browser still work: delete data there, then reclaim the space.",
  },
};

/** A storage lock banner on every project page (V2 §13). */
export function StorageBanner({ p }: { p: Project }) {
  const s = p.storage_state && STATE_TEXT[p.storage_state];
  if (!s) return null;
  return (
    <div
      className={cx(
        "mb-4 flex flex-wrap items-center justify-between gap-2 rounded-md border px-3 py-2 text-sm",
        s.tone === "warn" ? "border-warn/40 bg-warn/10 text-warn-text" : "border-danger/40 bg-danger/10 text-danger-text",
      )}
      role="alert"
      data-testid="storage-banner"
    >
      <span>
        <strong>{s.title}.</strong> {s.body}
      </span>
      {p.my_role === "admin" && (
        <Link to="/projects/$id/settings/database" params={{ id: p.id }} hash="storage" className="font-medium underline">
          Reclaim space
        </Link>
      )}
    </div>
  );
}

/** Size against the limit, the largest tables, and Reclaim space (V2 §10.4). */
export function StorageCard({ p }: { p: Project }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["project", p.id, "storage"], queryFn: () => api.projectStorage(p.id), enabled: p.tier === "shared" });
  const [opId, setOpId] = useState<string | undefined>();
  const [err, setErr] = useState<string | null>(null);
  const stream = useOperationStream(opId);
  useEffect(() => {
    if (stream.done) void qc.invalidateQueries({ queryKey: ["project", p.id] });
  }, [stream.done, qc, p.id]);
  if (p.tier !== "shared" || !q.data) return null;
  const s = q.data;
  const ratio = s.size_bytes != null && s.limit_bytes ? s.size_bytes / s.limit_bytes : null;
  const reclaim = async (schema: string, table: string) => {
    if (!window.confirm(`VACUUM FULL ${schema}.${table}? The table is locked (no reads or writes) until it finishes.`)) return;
    setErr(null);
    try {
      setOpId((await api.reclaimSpace(p.id, schema, table)).id);
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  return (
    <Panel title="Storage">
      <div id="storage" className="flex flex-col gap-3" data-testid="storage-card">
        <div className="flex items-center justify-between text-sm">
          <span>
            {s.size_bytes != null ? formatBytes(s.size_bytes) : "Not measured yet"}
            {s.limit_bytes ? ` of ${formatBytes(s.limit_bytes)}` : " (no limit)"}
          </span>
          {s.state !== "none" && (
            <Badge tone={s.state === "warn" ? "warn" : "danger"}>{s.state === "warn" ? "nearly full" : s.state === "soft" ? "read-only" : "offline"}</Badge>
          )}
        </div>
        {ratio != null && (
          <div className="h-1.5 overflow-hidden rounded-full bg-surface-2">
            <div
              className={cx("h-full rounded-full", ratio >= 1 ? "bg-danger" : ratio >= 0.9 ? "bg-warn" : "bg-accent")}
              style={{ width: `${Math.min(100, Math.max(2, ratio * 100))}%` }}
            />
          </div>
        )}
        <p className="text-xs text-muted">
          Deleted rows keep their space until the table is rewritten. Reclaim space runs VACUUM FULL on one table, which locks it while it runs; locks lift at
          the next size check (every 5 minutes).
        </p>
        {s.tables.length > 0 && (
          <Table head={["Table", "Size", "Dead rows", ""]}>
            {s.tables.map((t) => (
              <tr key={`${t.schema}.${t.table}`}>
                <td className="px-3 py-1.5 font-mono text-xs">
                  {t.schema}.{t.table}
                </td>
                <td className="px-3 py-1.5 text-xs">{formatBytes(t.bytes)}</td>
                <td className="px-3 py-1.5 text-xs">{t.dead_rows.toLocaleString()}</td>
                <td className="px-3 py-1.5 text-right">
                  <Button className="text-xs" onClick={() => reclaim(t.schema, t.table)} disabled={!!opId && !stream.done} data-testid={`reclaim-${t.table}`}>
                    Reclaim space
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        )}
        {err && <Alert>{err}</Alert>}
        {opId && <OperationLog log={stream.log} live={!stream.done} />}
      </div>
    </Panel>
  );
}

/** Switch a V1 project to opaque credentials (V2 §10.2). */
export function SwitchCredentialsCard({ p }: { p: Project }) {
  const qc = useQueryClient();
  const [days, setDays] = useState(7);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState<SwitchedCredentials | null>(null);
  if (!p.can_switch_credentials && !p.legacy_credentials_until && !done) return null;
  const run = async () => {
    setBusy(true);
    setErr(null);
    try {
      setDone(await api.switchCredentials(p.id, days));
      await qc.invalidateQueries({ queryKey: ["project", p.id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel title="Opaque credentials">
      <div className="flex flex-col gap-3" data-testid="switch-credentials">
        {done ? (
          <>
            <Alert tone="warn">Shown once. Update your apps; the old credentials stop working {formatDate(done.legacy_until)}.</Alert>
            <CopyField label="Pooled URL (transaction mode)" value={done.connection.pooled_url} secret testId="switched-pooled-url" />
            <CopyField label="Session URL" value={done.connection.session_url} secret testId="switched-session-url" />
          </>
        ) : p.legacy_credentials_until ? (
          <p className="text-sm">
            Switched. The old credentials keep working until {formatDate(p.legacy_credentials_until)} ({relativeTime(p.legacy_credentials_until)}).
          </p>
        ) : (
          <>
            <p className="text-sm text-muted">
              This project was created before Xenibase used opaque names, so its role name ({p.owner_role}) is visible to other tenants on the same cluster.
              Switching gives it an opaque role and a new password; the old ones keep working for a grace period, then stop. Members' personal database logins
              are renamed straight away (same password, new user name), so they need to copy the new connection string.
            </p>
            <div className="flex items-end gap-2">
              <Field label="Grace period (days)">
                {(id) => <Input id={id} type="number" min={1} max={90} className="w-24" value={days} onChange={(e) => setDays(Number(e.target.value))} />}
              </Field>
              <Button variant="primary" busy={busy} onClick={run} data-testid="switch-credentials-run">
                Switch to opaque credentials
              </Button>
            </div>
          </>
        )}
        {err && <Alert>{err}</Alert>}
      </div>
    </Panel>
  );
}

/** Statements and idle transactions the reaper ended (V2 §10.4). */
export function ReapedCard({ p }: { p: Project }) {
  const q = useQuery({ queryKey: ["project", p.id, "reaped"], queryFn: () => api.reapedSessions(p.id), enabled: p.tier === "shared" });
  if (!q.data || q.data.items.length === 0) return null;
  return (
    <Panel title="Ended by Xenibase">
      <p className="mb-2 text-xs text-muted">
        On the shared tier, statements running over 10 minutes are cancelled (and ended with their session if they keep running), and sessions idle in a transaction over 5 minutes are ended.
      </p>
      <Table head={["When", "What", "Role", "Ran for", "Query"]}>
        {q.data.items.map((r, i) => (
          <tr key={i} data-testid="reaped-session">
            <td className="px-3 py-1.5 text-xs">{relativeTime(r.created_at)}</td>
            <td className="px-3 py-1.5 text-xs">{r.kind === "statement" ? "statement cancelled" : r.kind === "statement_terminated" ? "statement ended (it ignored the cancel)" : "idle transaction ended"}</td>
            <td className="px-3 py-1.5 font-mono text-xs">{r.role}</td>
            <td className="px-3 py-1.5 text-xs">{Math.round(r.duration_s / 60)} min</td>
            <td className="max-w-xs truncate px-3 py-1.5 font-mono text-xs" title={r.query ?? ""}>
              {r.query ?? "—"}
            </td>
          </tr>
        ))}
      </Table>
    </Panel>
  );
}
