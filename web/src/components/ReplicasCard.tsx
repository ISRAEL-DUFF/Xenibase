import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import {
  api,
  errorMessage,
  type Project,
  type ProjectCredentials,
  type ReadReplica,
} from "../api/client";
import { formatBytes, relativeTime } from "../lib/format";
import { useOperationStream } from "../lib/useOperationStream";
import { CostEstimate } from "./CostEstimate";
import { OperationLog } from "./OperationLog";
import { ProvisionProgress } from "./ProvisionProgress";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  CopyButton,
  Field,
  Input,
  Panel,
  Select,
  SidePanel,
  StatusBadge,
  Table,
} from "./ui";

const statusTone: Record<
  ReadReplica["status"],
  "ok" | "warn" | "danger" | "muted"
> = {
  creating: "muted",
  streaming: "ok",
  lagging: "warn",
  down: "danger",
  deleting: "muted",
  detaching: "muted",
  failed: "danger",
};

function lag(r: ReadReplica): string {
  if (r.lag_ms == null) return "—";
  const t =
    r.lag_ms < 1000 ? `${r.lag_ms} ms` : `${(r.lag_ms / 1000).toFixed(1)} s`;
  return r.lag_bytes ? `${t} (${formatBytes(r.lag_bytes)})` : t;
}

type Standby = { feedback: boolean; delay: string };

const defaultStandby: Standby = { feedback: false, delay: "30" };

function standbyOf(r: ReadReplica): Standby {
  return {
    feedback: r.hot_standby_feedback,
    delay: String(r.max_standby_delay_seconds),
  };
}

function standbyDelay(s: Standby): number | null {
  const n = Number(s.delay);
  return Number.isInteger(n) && n >= 0 && n <= 900 ? n : null;
}

/** A replica's standby settings: hot_standby_feedback and how long replay
 * waits for a query that conflicts with it. */
function StandbyFields({
  value,
  onChange,
}: {
  value: Standby;
  onChange: (s: Standby) => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      <label className="flex items-start gap-2">
        <Checkbox
          checked={value.feedback}
          onCheckedChange={(c) => onChange({ ...value, feedback: c === true })}
          aria-label="Hot standby feedback"
          data-testid="replica-feedback"
          className="mt-0.5"
        />
        <span>
          <span className="font-medium">Hot standby feedback</span>
          <span className="block text-muted">
            The primary keeps the rows this replica's queries still read, so
            long queries here aren't cancelled. Dead rows stay longer on the
            primary, so a very long query here can bloat its tables.
          </span>
        </span>
      </label>
      <Field
        label="Wait for conflicting queries (seconds)"
        hint="How long replay waits for a query that conflicts with it before cancelling the query: 0 to 900, Postgres's default is 30. Longer lets the replica fall further behind meanwhile."
      >
        {(id) => (
          <Input
            id={id}
            type="number"
            min={0}
            max={900}
            value={value.delay}
            onChange={(e) => onChange({ ...value, delay: e.target.value })}
            data-testid="replica-delay"
          />
        )}
      </Field>
    </div>
  );
}

/**
 * Read replicas of a dedicated project (V4 §7): streaming copies on other
 * nodes, never promoted, that the pooler's read-only route balances reads
 * across while they keep up.
 */
export function ReplicasCard({ p }: { p: Project }) {
  const qc = useQueryClient();
  const busyStatus = p.status !== "active";
  const list = useQuery({
    queryKey: ["replicas", p.id],
    queryFn: () => api.replicas(p.id),
    refetchInterval: 5000,
    enabled: p.tier === "dedicated",
  });
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: api.nodes,
    retry: false,
  });
  const [open, setOpen] = useState(false);
  const [nodeId, setNodeId] = useState("");
  const [detaching, setDetaching] = useState<ReadReplica | null>(null);
  const [standby, setStandby] = useState<Standby>(defaultStandby);
  const [editing, setEditing] = useState<ReadReplica | null>(null);
  const [edit, setEdit] = useState<Standby>(defaultStandby);
  const [name, setName] = useState("");
  const [created, setCreated] = useState<ProjectCredentials | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [opId, setOpId] = useState<string | undefined>();
  const stream = useOperationStream(opId);

  const run = async (what: string, f: () => Promise<{ id: string }>) => {
    setBusy(what);
    setErr(null);
    try {
      const op = await f();
      setOpId(op.id);
      setOpen(false);
      await qc.invalidateQueries({ queryKey: ["replicas", p.id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const detach = async () => {
    if (!detaching) return;
    setBusy("detach");
    setErr(null);
    try {
      const c = await api.detachReplica(p.id, detaching.id, name);
      setDetaching(null);
      setCreated(c);
      await qc.invalidateQueries({ queryKey: ["projects"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const saveSettings = async () => {
    const delay = standbyDelay(edit);
    if (!editing || delay == null) return;
    setBusy("settings");
    setErr(null);
    try {
      await api.updateReplica(p.id, editing.id, {
        hot_standby_feedback: edit.feedback,
        max_standby_delay_seconds: delay,
      });
      setEditing(null);
      await qc.invalidateQueries({ queryKey: ["replicas", p.id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  if (p.tier !== "dedicated") return null;
  if (created)
    return (
      <Panel title="Read replicas">
        <ProvisionProgress creds={created} progressTitle="Detach" />
      </Panel>
    );
  const l = list.data;
  const reps = l?.replicas ?? [];
  const used = new Set([p.instance?.node_id, ...reps.map((r) => r.node_id)]);
  const targets = (nodes.data?.items ?? []).filter(
    (n) =>
      (n.role === "dedicated" || n.role === "both") &&
      n.agent.registered &&
      !used.has(n.id),
  );
  const full = l ? reps.length >= l.max_replicas : true;

  return (
    <Panel
      title="Read replicas"
      actions={
        opId && stream.status ? (
          <StatusBadge status={stream.status} />
        ) : (
          <Badge tone={reps.length ? "ok" : "muted"}>
            {reps.length} of {l?.max_replicas ?? 2}
          </Badge>
        )
      }
    >
      <div className="flex flex-col gap-4 text-sm" data-testid="replicas">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-muted">
            Copies on other nodes that serve reads. Connect to the read-only
            route to use them; a replica more than{" "}
            {((l?.max_lag_ms ?? 10000) / 1000).toFixed(0)} s behind leaves
            rotation until it catches up, and they follow a new primary after a
            failover.
          </p>
          <Button
            onClick={() => setOpen(true)}
            disabled={busyStatus || full}
            title={full ? "A project has at most 2 read replicas" : undefined}
          >
            Add replica…
          </Button>
        </div>
        {l?.read_url && (
          <div className="flex items-center gap-2" data-testid="replica-url">
            <span className="font-medium">Read-only URL</span>
            <code className="truncate rounded bg-subtle px-2 py-1 text-xs">
              {l.read_url}
            </code>
            <CopyButton value={l.read_url} />
          </div>
        )}
        {reps.length > 0 && (
          <Table
            head={[
              "Node",
              "Region",
              "Status",
              "In rotation",
              "Behind",
              "Standby",
              "",
            ]}
          >
            {reps.map((r) => (
              <tr key={r.id} data-testid="replica" data-status={r.status}>
                <td className="px-3 py-2">{r.node_name}</td>
                <td className="px-3 py-2 text-muted">{r.region}</td>
                <td className="px-3 py-2">
                  <Badge tone={statusTone[r.status]}>{r.status}</Badge>
                </td>
                <td className="px-3 py-2">
                  {r.in_rotation ? "Yes" : "No"}
                  {r.rotation_changed_at && (
                    <span className="block text-xs text-muted">
                      since {relativeTime(r.rotation_changed_at)}
                    </span>
                  )}
                </td>
                <td className="px-3 py-2">{lag(r)}</td>
                <td className="px-3 py-2" data-testid="replica-standby">
                  {r.hot_standby_feedback ? "Feedback on" : "Feedback off"}
                  <span className="block text-xs text-muted">
                    waits {r.max_standby_delay_seconds} s
                  </span>
                  {r.settings_error && (
                    <span className="block text-xs text-warn-text">
                      not applied yet: {r.settings_error}
                    </span>
                  )}
                </td>
                <td className="px-3 py-2 text-right">
                  <div className="flex justify-end gap-2">
                    <Button
                      onClick={() => {
                        setEdit(standbyOf(r));
                        setEditing(r);
                      }}
                      disabled={
                        r.status === "deleting" || r.status === "detaching"
                      }
                    >
                      Settings…
                    </Button>
                    <Button
                      onClick={() => {
                        setName(`${p.name} replica`);
                        setDetaching(r);
                      }}
                      disabled={
                        busyStatus ||
                        r.status === "creating" ||
                        r.status === "detaching"
                      }
                    >
                      Detach…
                    </Button>
                    <Button
                      variant="danger"
                      busy={busy === r.id}
                      disabled={busyStatus || r.status === "detaching"}
                      onClick={() =>
                        run(r.id, () => api.deleteReplica(p.id, r.id))
                      }
                    >
                      Remove
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </Table>
        )}
        {opId && <OperationLog log={stream.log} live={!stream.done} />}
        {stream.status === "failed" && <Alert>{stream.error}</Alert>}
        {err && <Alert>{err}</Alert>}
      </div>
      <SidePanel
        open={open}
        onOpenChange={setOpen}
        title="Add a read replica"
        footer={
          <>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              busy={busy === "create"}
              disabled={standbyDelay(standby) == null}
              onClick={() =>
                run("create", () =>
                  api.createReplica(p.id, {
                    node_id: nodeId || undefined,
                    hot_standby_feedback: standby.feedback,
                    max_standby_delay_seconds: standbyDelay(standby) ?? 30,
                  }),
                )
              }
            >
              Add replica
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4 text-sm">
          <Field
            label="Node"
            hint="Any node without another copy of this database."
          >
            {(id) => (
              <Select
                id={id}
                value={nodeId}
                onChange={(e) => setNodeId(e.target.value)}
              >
                <option value="">Least loaded</option>
                {targets.map((n) => (
                  <option
                    key={n.id}
                    value={n.id}
                    disabled={n.status !== "healthy"}
                  >
                    {n.name} ({n.region})
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <StandbyFields value={standby} onChange={setStandby} />
          <ol className="list-decimal pl-5 text-muted">
            <li>
              If the project isn't under Patroni yet, the database restarts once
              (clients on the pooled URL wait a few seconds).
            </li>
            <li>
              The replica is restored from the latest backup and streams
              changes.
            </li>
            <li>
              Reads through the read-only route go to it once it keeps up. It
              bills like a dedicated instance of this project's size.
            </li>
          </ol>
          <CostEstimate
            org={p.org_id}
            what="the replica"
            req={
              p.instance?.cpus != null
                ? {
                    cpus: p.instance.cpus,
                    memory_mb: p.instance.memory_mb ?? 0,
                    disk_gb: p.instance.volume_gb ?? 0,
                  }
                : null
            }
          />
        </div>
      </SidePanel>
      <SidePanel
        open={!!editing}
        onOpenChange={(o) => !o && setEditing(null)}
        title="Replica settings"
        footer={
          <>
            <Button onClick={() => setEditing(null)}>Cancel</Button>
            <Button
              variant="primary"
              busy={busy === "settings"}
              disabled={standbyDelay(edit) == null}
              onClick={saveSettings}
            >
              Save
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4 text-sm">
          <p className="text-muted">
            For the replica on {editing?.node_name} only. Applied at once with a
            configuration reload: no restart, and connections stay open.
          </p>
          <StandbyFields value={edit} onChange={setEdit} />
        </div>
      </SidePanel>
      <SidePanel
        open={!!detaching}
        onOpenChange={(o) => !o && setDetaching(null)}
        title="Detach into a project"
        footer={
          <>
            <Button onClick={() => setDetaching(null)}>Cancel</Button>
            <Button
              variant="primary"
              busy={busy === "detach"}
              disabled={!name.trim()}
              onClick={detach}
            >
              Detach
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4 text-sm">
          <p className="text-muted">
            The replica on {detaching?.node_name} stops following this project
            and becomes a dedicated project of its own, with its data as of now
            and a new password. This project is not changed.
          </p>
          <Field label="New project name">
            {(id) => (
              <Input
                id={id}
                required
                maxLength={64}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            )}
          </Field>
        </div>
      </SidePanel>
    </Panel>
  );
}
