import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useStepUp } from "../components/StepUp";
import {
  api,
  errorMessage,
  type CapacitySettings,
  type Node,
} from "../api/client";
import {
  Alert,
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  Page,
  PageSkeleton,
  Panel,
  Select,
  SidePanel,
  Table,
  type Tone,
} from "../components/ui";
import { formatBytes, formatDate, relativeTime } from "../lib/format";

const PROPOSAL_TONE: Record<string, Tone> = {
  pending: "warn",
  approved: "accent",
  provisioning: "accent",
  done: "ok",
  rejected: "muted",
  failed: "danger",
  superseded: "muted",
};
const MOVE_TONE: Record<string, Tone> = {
  proposed: "warn",
  approved: "accent",
  moving: "accent",
  done: "ok",
  failed: "danger",
  skipped: "muted",
  rejected: "muted",
};

function money(minor: number | null | undefined, currency = "EUR"): string {
  if (minor == null) return "—";
  return `${currency} ${(minor / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** Platform → Capacity (V3 §5): what the planner sees, its proposals,
 * drains and rebalancing, and the nodes with what they cost. */
export function AdminCapacityPage() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["admin", "capacity"],
    queryFn: api.capacity,
    refetchInterval: 10_000,
  });
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [costFor, setCostFor] = useState<Node | null>(null);
  const stepUp = useStepUp();
  const act = async (key: string, f: () => Promise<unknown>, done?: string) => {
    setBusy(key);
    setErr(null);
    setNote(null);
    try {
      await stepUp.run(f);
      await qc.invalidateQueries({ queryKey: ["admin", "capacity"] });
      if (done) setNote(done);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  if (q.isPending) return <PageSkeleton />;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const c = q.data;
  const st = c.settings;
  const batches = [
    ...new Set(
      c.moves
        .filter((m) => m.kind === "rebalance" && m.status === "proposed")
        .map((m) => m.batch),
    ),
  ];
  return (
    <Page
      title="Capacity"
      description={
        c.can_create
          ? `Servers are created on ${c.provider} when a threshold trips and the nodes' monthly cost stays within the budget; others wait for you. Empty servers are deleted after ${st.delete_empty_after_hours} hours.`
          : "Servers are registered by hand (no provider API): proposals tell you when to add one."
      }
      testId="admin-capacity"
      actions={
        <>
          <Button
            busy={busy === "evaluate"}
            onClick={() =>
              void act("evaluate", api.evaluateCapacity, "Thresholds checked.")
            }
            data-testid="capacity-evaluate"
          >
            Check now
          </Button>
          <Button
            busy={busy === "rebalance"}
            onClick={() =>
              void act("rebalance", api.planRebalance, "Rebalancing planned.")
            }
          >
            Plan rebalance
          </Button>
          <Button
            variant="primary"
            onClick={() => setSettingsOpen(true)}
            data-testid="capacity-settings"
          >
            Settings
          </Button>
        </>
      }
    >
      {err && <Alert>{err}</Alert>}
      {note && <Alert tone="ok">{note}</Alert>}
      <div className="grid gap-3 md:grid-cols-2">
        {c.shared.map((r) => {
          const now = r.total_bytes ? r.used_bytes / r.total_bytes : 0;
          const proj = r.total_bytes ? r.projected_bytes / r.total_bytes : 0;
          return (
            <Panel
              key={`s-${r.region}`}
              title={`Shared disk · ${r.region}`}
              description={`${r.nodes} node${r.nodes === 1 ? "" : "s"}, ${formatBytes(r.total_bytes)}`}
              testId={`outlook-shared-${r.region}`}
            >
              <div className="relative h-3 overflow-hidden rounded-full bg-surface-2">
                <div
                  className="absolute inset-y-0 left-0 bg-accent/40"
                  style={{ width: `${Math.min(100, proj * 100)}%` }}
                />
                <div
                  className="absolute inset-y-0 left-0 bg-accent"
                  style={{ width: `${Math.min(100, now * 100)}%` }}
                />
                <div
                  className="absolute inset-y-0 w-0.5 bg-warn"
                  style={{ left: `${r.threshold * 100}%` }}
                />
              </div>
              <p className="mt-2 text-[13px] text-muted">
                {(now * 100).toFixed(0)}% used now; {(proj * 100).toFixed(0)}%
                in {r.horizon_days} days at the past week's growth. Threshold{" "}
                {(r.threshold * 100).toFixed(0)}%.
              </p>
            </Panel>
          );
        })}
        {c.dedicated
          .filter((r) => r.nodes > 0)
          .map((r) => (
            <Panel
              key={`d-${r.region}`}
              title={`Dedicated hosts · ${r.region}`}
              description={`${r.nodes} host${r.nodes === 1 ? "" : "s"}`}
            >
              <p className="text-[13px]">
                {r.fits_on ? (
                  <>
                    Room for a <strong>{r.largest}</strong> instance on{" "}
                    {r.fits_on}.
                  </>
                ) : (
                  <span className="text-warn-text">
                    No host has room for a {r.largest} instance (most free:{" "}
                    {r.free_cpus.toFixed(1)} vCPU, {r.free_mem_mb} MB).
                  </span>
                )}
              </p>
            </Panel>
          ))}
      </div>

      <Panel
        title="Proposals"
        description={`Monthly budget ${money(st.monthly_budget_minor, st.budget_currency)}; the nodes cost ${money(c.budget_used_minor, st.budget_currency)} a month.`}
        testId="capacity-proposals"
      >
        {c.proposals.length === 0 ? (
          <p className="text-sm text-muted">No proposals yet.</p>
        ) : (
          <Table head={["Proposal", "Server", "Monthly", "Status", ""]}>
            {c.proposals.map((p) => (
              <tr key={p.id} data-testid={`proposal-${p.status}`}>
                <td className="max-w-md px-3 py-2">
                  <div className="font-medium">
                    {p.region} · {p.tier}
                  </div>
                  <div className="text-xs text-muted">{p.reason}</div>
                  {p.error && (
                    <div className="text-xs text-danger-text">{p.error}</div>
                  )}
                </td>
                <td className="px-3 py-2 text-xs">
                  {p.server_type}
                  {p.location && <div className="text-muted">{p.location}</div>}
                  {p.node_name && (
                    <div className="text-muted">→ {p.node_name}</div>
                  )}
                </td>
                <td className="px-3 py-2 text-xs">
                  {money(p.monthly_cost_minor, p.currency)}
                </td>
                <td className="px-3 py-2">
                  <Badge tone={PROPOSAL_TONE[p.status] ?? "muted"}>
                    {p.status}
                  </Badge>{" "}
                  {p.auto && <Badge tone="muted">auto</Badge>}
                  <div className="text-xs text-muted">
                    {relativeTime(p.created_at)}
                  </div>
                </td>
                <td className="px-3 py-2">
                  {p.status === "pending" && (
                    <div className="flex justify-end gap-1">
                      <Button
                        size="tiny"
                        variant="primary"
                        busy={busy === p.id}
                        onClick={() =>
                          void act(p.id, () => api.approveProposal(p.id))
                        }
                        data-testid="proposal-approve"
                      >
                        {c.can_create ? "Provision" : "Mark added"}
                      </Button>
                      <Button
                        size="tiny"
                        onClick={() =>
                          void act(p.id, () => api.rejectProposal(p.id))
                        }
                      >
                        Reject
                      </Button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>

      <Panel
        title="Drains and rebalancing"
        description="One move at a time, with zero-downtime moves. Rebalance batches wait for you unless automatic rebalancing is on (then they run in the maintenance window)."
      >
        {batches.map((b) => (
          <div
            key={b}
            className="mb-3 flex flex-wrap items-center justify-between gap-2 rounded-md border border-warn/40 bg-warn/5 px-3 py-2 text-sm"
          >
            <span>
              A rebalance batch of{" "}
              {
                c.moves.filter((m) => m.batch === b && m.status === "proposed")
                  .length
              }{" "}
              moves waits for approval.
            </span>
            <span className="flex gap-1">
              <Button
                size="tiny"
                variant="primary"
                onClick={() => void act(b, () => api.decideBatch(b, true))}
              >
                Approve
              </Button>
              <Button
                size="tiny"
                onClick={() => void act(b, () => api.decideBatch(b, false))}
              >
                Reject
              </Button>
            </span>
          </div>
        ))}
        {c.moves.length === 0 ? (
          <p className="text-sm text-muted">Nothing is being moved.</p>
        ) : (
          <Table head={["Project", "From", "To", "Why", "Status"]}>
            {c.moves.map((m) => (
              <tr key={m.id}>
                <td className="px-3 py-2">{m.project_name}</td>
                <td className="px-3 py-2 text-xs">{m.from_name}</td>
                <td className="px-3 py-2 text-xs">
                  {m.to_name ?? "chosen when it moves"}
                </td>
                <td className="px-3 py-2 text-xs text-muted">
                  {m.kind}: {m.reason}
                  {m.error && <div className="text-danger-text">{m.error}</div>}
                </td>
                <td className="px-3 py-2">
                  <Badge tone={MOVE_TONE[m.status] ?? "muted"}>
                    {m.status}
                  </Badge>
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>

      <Panel title="Nodes" testId="capacity-nodes">
        {c.nodes.length === 0 ? (
          <EmptyState title="No nodes yet" />
        ) : (
          <Table
            head={["Node", "Region", "Source", "Monthly cost", "State", ""]}
          >
            {c.nodes.map((n) => (
              <tr key={n.id} data-testid={`capacity-node-${n.name}`}>
                <td className="px-3 py-2">
                  <div className="font-medium">{n.name}</div>
                  <div className="text-xs text-muted">
                    {n.role} · {n.private_addr}
                  </div>
                </td>
                <td className="px-3 py-2 text-xs">{n.region}</td>
                <td className="px-3 py-2 text-xs">
                  {n.provider}
                  {n.server_type && (
                    <div className="text-muted">{n.server_type}</div>
                  )}
                </td>
                <td className="px-3 py-2 text-xs">
                  {money(n.monthly_cost_minor, n.cost_currency)}
                </td>
                <td className="px-3 py-2">
                  <Badge
                    tone={
                      n.lifecycle === "active"
                        ? "ok"
                        : n.lifecycle === "draining" ||
                            n.lifecycle === "retiring"
                          ? "warn"
                          : "accent"
                    }
                  >
                    {n.lifecycle}
                  </Badge>
                  {n.empty_since && (
                    <div className="text-xs text-muted">
                      empty since {formatDate(n.empty_since)}
                    </div>
                  )}
                  {n.keep && <div className="text-xs text-muted">kept</div>}
                </td>
                <td className="px-3 py-2">
                  <div className="flex justify-end gap-1">
                    <Button size="tiny" onClick={() => setCostFor(n)}>
                      Cost
                    </Button>
                    {n.lifecycle === "draining" ? (
                      <Button
                        size="tiny"
                        busy={busy === n.id}
                        onClick={() =>
                          void act(
                            n.id,
                            () => api.stopDrain(n.id),
                            `${n.name} is back in service.`,
                          )
                        }
                      >
                        Stop drain
                      </Button>
                    ) : (
                      <Button
                        size="tiny"
                        variant="danger"
                        busy={busy === n.id}
                        onClick={() =>
                          void act(
                            n.id,
                            () => api.drainNode(n.id),
                            `Draining ${n.name}.`,
                          )
                        }
                        data-testid={`drain-${n.name}`}
                      >
                        Drain
                      </Button>
                    )}
                  </div>
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
      <SettingsPanel
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        settings={st}
        run={stepUp.run}
      />
      <NodeCostPanel node={costFor} onClose={() => setCostFor(null)} />
      {stepUp.dialog}
    </Page>
  );
}

function SettingsPanel({
  open,
  onClose,
  settings,
  run,
}: {
  open: boolean;
  onClose: () => void;
  settings: CapacitySettings;
  run: (f: () => Promise<unknown>) => Promise<void>;
}) {
  const qc = useQueryClient();
  const [s, setS] = useState<CapacitySettings>(settings);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await run(async () => {
        await api.saveCapacitySettings(s);
        await qc.invalidateQueries({ queryKey: ["admin", "capacity"] });
        onClose();
      });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const num = (v: string) => (v === "" ? 0 : Number(v));
  const edge = s.edge ?? { enabled: true, cpu_threshold: 70 };
  return (
    <SidePanel
      open={open}
      onOpenChange={(o) => {
        if (o) setS(settings);
        else onClose();
      }}
      title="Capacity settings"
      description="Thresholds per tier, the server each adds, and the budget within which Xenibase provisions by itself."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            form="capacity-form"
            variant="primary"
            busy={busy}
            data-testid="capacity-save"
          >
            Save
          </Button>
        </>
      }
    >
      <form id="capacity-form" className="flex flex-col gap-4" onSubmit={save}>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={s.auto_apply}
            onChange={(e) => setS({ ...s, auto_apply: e.target.checked })}
          />{" "}
          Provision within the budget by itself
        </label>
        <div className="grid grid-cols-[1fr_6rem] gap-2">
          <Field
            label="Monthly infrastructure budget"
            hint="The nodes' monthly cost plus a proposal's must stay within it."
          >
            {(id) => (
              <Input
                id={id}
                inputMode="decimal"
                value={String(s.monthly_budget_minor / 100)}
                onChange={(e) =>
                  setS({
                    ...s,
                    monthly_budget_minor: Math.round(num(e.target.value) * 100),
                  })
                }
                data-testid="capacity-budget"
              />
            )}
          </Field>
          <Field label="Currency">
            {(id) => (
              <Input
                id={id}
                value={s.budget_currency}
                maxLength={3}
                onChange={(e) =>
                  setS({ ...s, budget_currency: e.target.value.toUpperCase() })
                }
              />
            )}
          </Field>
        </div>
        <h3 className="text-[14px]">Shared tier</h3>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={s.shared.enabled}
            onChange={(e) =>
              setS({ ...s, shared: { ...s.shared, enabled: e.target.checked } })
            }
          />{" "}
          Propose shared nodes
        </label>
        <div className="grid grid-cols-2 gap-2">
          <Field label="Disk threshold (%)">
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(
                  Math.round((s.shared.disk_threshold ?? 0.7) * 100),
                )}
                onChange={(e) =>
                  setS({
                    ...s,
                    shared: {
                      ...s.shared,
                      disk_threshold: num(e.target.value) / 100,
                    },
                  })
                }
              />
            )}
          </Field>
          <Field label="Within (days)">
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(s.shared.horizon_days ?? 14)}
                onChange={(e) =>
                  setS({
                    ...s,
                    shared: { ...s.shared, horizon_days: num(e.target.value) },
                  })
                }
              />
            )}
          </Field>
          <Field label="Server type" hint="Empty: the cheapest that fits.">
            {(id) => (
              <Input
                id={id}
                value={s.shared.server_type ?? ""}
                onChange={(e) =>
                  setS({
                    ...s,
                    shared: { ...s.shared, server_type: e.target.value },
                  })
                }
              />
            )}
          </Field>
          <Field label="Shared cluster memory (MB)">
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(s.shared.cluster_memory_mb ?? 2048)}
                onChange={(e) =>
                  setS({
                    ...s,
                    shared: {
                      ...s.shared,
                      cluster_memory_mb: num(e.target.value),
                    },
                  })
                }
              />
            )}
          </Field>
        </div>
        <h3 className="text-[14px]">Dedicated tier</h3>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={s.dedicated.enabled}
            onChange={(e) =>
              setS({
                ...s,
                dedicated: { ...s.dedicated, enabled: e.target.checked },
              })
            }
          />{" "}
          Propose a host when the largest size doesn't fit
        </label>
        <Field label="Server type">
          {(id) => (
            <Input
              id={id}
              value={s.dedicated.server_type ?? ""}
              onChange={(e) =>
                setS({
                  ...s,
                  dedicated: { ...s.dedicated, server_type: e.target.value },
                })
              }
            />
          )}
        </Field>
        <h3 className="text-[14px]">Edge tier</h3>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={edge.enabled}
            onChange={(e) =>
              setS({ ...s, edge: { ...edge, enabled: e.target.checked } })
            }
          />{" "}
          Propose an edge node when a region's edges stay busy for an hour
        </label>
        <div className="grid grid-cols-2 gap-2">
          <Field
            label="Edge CPU threshold (%)"
            hint="The edges' average share of their hosts' CPUs."
          >
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(edge.cpu_threshold ?? 70)}
                onChange={(e) =>
                  setS({
                    ...s,
                    edge: { ...edge, cpu_threshold: num(e.target.value) },
                  })
                }
              />
            )}
          </Field>
          <Field label="Server type">
            {(id) => (
              <Input
                id={id}
                value={edge.server_type ?? ""}
                onChange={(e) =>
                  setS({
                    ...s,
                    edge: { ...edge, server_type: e.target.value },
                  })
                }
              />
            )}
          </Field>
        </div>
        <h3 className="text-[14px]">Rebalancing and empty nodes</h3>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={s.auto_rebalance}
            onChange={(e) => setS({ ...s, auto_rebalance: e.target.checked })}
          />{" "}
          Rebalance in the maintenance window without approval
        </label>
        <div className="grid grid-cols-2 gap-2">
          <Field
            label="Spread (%)"
            hint="Propose moves when shared nodes' disk use differs by more."
          >
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(Math.round(s.rebalance_spread * 100))}
                onChange={(e) =>
                  setS({ ...s, rebalance_spread: num(e.target.value) / 100 })
                }
              />
            )}
          </Field>
          <Field label="Delete empty servers after (hours)">
            {(id) => (
              <Input
                id={id}
                inputMode="numeric"
                value={String(s.delete_empty_after_hours)}
                onChange={(e) =>
                  setS({ ...s, delete_empty_after_hours: num(e.target.value) })
                }
              />
            )}
          </Field>
        </div>
        {err && <Alert>{err}</Alert>}
      </form>
    </SidePanel>
  );
}

function NodeCostPanel({
  node,
  onClose,
}: {
  node: Node | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [cost, setCost] = useState("");
  const [currency, setCurrency] = useState("EUR");
  const [region, setRegion] = useState("");
  const [keep, setKeep] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!node) return;
    setBusy(true);
    setErr(null);
    try {
      await api.setNodeCost(node.id, {
        monthly_cost_minor: cost === "" ? null : Math.round(Number(cost) * 100),
        currency,
        region: region || undefined,
        keep,
      });
      await qc.invalidateQueries({ queryKey: ["admin", "capacity"] });
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SidePanel
      open={!!node}
      onOpenChange={(o) => {
        if (o && node) {
          setCost(
            node.monthly_cost_minor != null
              ? String(node.monthly_cost_minor / 100)
              : "",
          );
          setCurrency(node.cost_currency ?? "EUR");
          setRegion(node.region ?? "");
          setKeep(!!node.keep);
        } else onClose();
      }}
      title={`${node?.name ?? ""}: cost`}
      description="What the machine costs a month: cost attribution divides it among the projects on it."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            form="node-cost-form"
            variant="primary"
            busy={busy}
          >
            Save
          </Button>
        </>
      }
    >
      <form id="node-cost-form" className="flex flex-col gap-4" onSubmit={save}>
        <div className="grid grid-cols-[1fr_6rem] gap-2">
          <Field label="Monthly cost">
            {(id) => (
              <Input
                id={id}
                inputMode="decimal"
                value={cost}
                onChange={(e) => setCost(e.target.value)}
              />
            )}
          </Field>
          <Field label="Currency">
            {(id) => (
              <Select
                id={id}
                value={currency}
                onChange={(e) => setCurrency(e.target.value)}
              >
                {["EUR", "USD", "NGN", "GBP"].map((c) => (
                  <option key={c}>{c}</option>
                ))}
              </Select>
            )}
          </Field>
        </div>
        <Field label="Region">
          {(id) => (
            <Input
              id={id}
              value={region}
              onChange={(e) => setRegion(e.target.value)}
            />
          )}
        </Field>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={keep}
            onChange={(e) => setKeep(e.target.checked)}
          />{" "}
          Keep it even when empty
        </label>
        {err && <Alert>{err}</Alert>}
      </form>
    </SidePanel>
  );
}
