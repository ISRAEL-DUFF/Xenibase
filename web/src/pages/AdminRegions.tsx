import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import {
  api,
  errorMessage,
  type AdminRegion,
  type AdminRegionRequest,
} from "../api/client";
import {
  Alert,
  Badge,
  Button,
  Field,
  Input,
  Page,
  PageSkeleton,
  Panel,
  Select,
  SidePanel,
  Stat,
  Table,
} from "../components/ui";

const COPY_LABEL: Record<string, string> = {
  copied: "Copied",
  pending: "Pending",
  failed: "Failed",
  skipped: "Skipped (residency)",
  none: "Not copied",
};

/** Platform → Regions (V3 §6): each region's pooler hostname, storage and
 * copy targets, data residency, and how its backups are copied. */
export function AdminRegionsPage() {
  const regions = useQuery({
    queryKey: ["admin", "regions"],
    queryFn: api.adminRegions,
  });
  const targets = useQuery({
    queryKey: ["storage-targets", "platform"],
    queryFn: () => api.storageTargets(),
  });
  const [editing, setEditing] = useState<AdminRegion | "new" | null>(null);
  if (!regions.data) return <PageSkeleton />;
  const targetName = (id?: string | null) =>
    id
      ? (targets.data?.items.find((t) => t.id === id)?.name ?? id.slice(0, 8))
      : "Platform default";
  const copies = regions.data.copies;
  return (
    <Page
      title="Regions"
      description="Where projects run. Each region has its own pooler hostname, backup target and cross-region copy target."
      testId="admin-regions"
      actions={
        <Button variant="primary" onClick={() => setEditing("new")}>
          Add region
        </Button>
      }
    >
      <Panel title="Regions">
        <Table
          head={[
            "Region",
            "Hostname",
            "Backups",
            "Copies to",
            "Files",
            "Residency",
            "Nodes",
            "Projects",
            "",
          ]}
        >
          {regions.data.items.map((r) => (
            <tr key={r.id} data-testid={`region-${r.id}`}>
              <td className="px-3 py-2">
                <div className="font-medium">
                  {r.name}{" "}
                  {r.country && (
                    <span className="text-muted">({r.country})</span>
                  )}
                </div>
                <div className="text-xs text-muted">
                  {r.id}
                  {r.home && " · home"}
                  {r.status === "hidden" && " · hidden"}
                </div>
              </td>
              <td className="px-3 py-2">
                <div className="font-mono text-xs">
                  {r.pooler_host || "platform host"}
                </div>
                <div className="text-xs text-muted">
                  {r.home || r.pooler_hosts > 0
                    ? `${r.pooler_hosts} pooler host${r.pooler_hosts === 1 ? "" : "s"}`
                    : "served by the home poolers"}
                </div>
              </td>
              <td className="px-3 py-2">{targetName(r.storage_target_id)}</td>
              <td className="px-3 py-2">
                {r.copy_target_id ? (
                  targetName(r.copy_target_id)
                ) : (
                  <span className="text-muted">No copies</span>
                )}
              </td>
              <td className="px-3 py-2">
                {r.files_target_id ? (
                  targetName(r.files_target_id)
                ) : (
                  <Badge tone="warn">In the backup target</Badge>
                )}
              </td>
              <td className="px-3 py-2">
                {r.residency ? (
                  <Badge tone="accent">
                    Offered ({r.residency_projects} on)
                  </Badge>
                ) : (
                  <span className="text-muted">No</span>
                )}
              </td>
              <td className="px-3 py-2">{r.nodes}</td>
              <td className="px-3 py-2">{r.projects}</td>
              <td className="px-3 py-2 text-right">
                <Button size="small" onClick={() => setEditing(r)}>
                  Edit
                </Button>
              </td>
            </tr>
          ))}
        </Table>
      </Panel>
      <Panel title="Cross-region backup copies (last 7 days)">
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
          {copies.length === 0 && (
            <span className="text-[13px] text-muted">
              No backups in the last 7 days.
            </span>
          )}
          {copies.map((c) => (
            <Stat
              key={c.status}
              label={COPY_LABEL[c.status] ?? c.status}
              value={String(c.count)}
            />
          ))}
        </div>
      </Panel>
      <RegionPanel
        key={editing === "new" ? "new" : (editing?.id ?? "none")}
        region={editing}
        targets={(targets.data?.items ?? []).filter(
          (t) => t.kind === "platform",
        )}
        onClose={() => setEditing(null)}
      />
    </Page>
  );
}

function RegionPanel({
  region,
  targets,
  onClose,
}: {
  region: AdminRegion | "new" | null;
  targets: { id: string; name: string }[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const r = region && region !== "new" ? region : null;
  const [id, setId] = useState(r?.id ?? "");
  const [f, setF] = useState<AdminRegionRequest>({
    name: r?.name ?? "",
    country: r?.country ?? "",
    pooler_host: r?.pooler_host ?? "",
    api_domain: r?.api_domain ?? "",
    provider: r?.provider ?? "manual",
    location: r?.location ?? "",
    storage_target_id: r?.storage_target_id ?? null,
    copy_target_id: r?.copy_target_id ?? null,
    files_target_id: r?.files_target_id ?? null,
    floating_ip_id: r?.floating_ip_id ?? null,
    residency: r?.residency ?? false,
    hidden: r?.status === "hidden",
  });
  const [early, setEarly] = useState(
    (r?.early_access_org_ids ?? []).join("\n"),
  );
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const set = (k: keyof AdminRegionRequest, v: unknown) =>
    setF((x) => ({ ...x, [k]: v }));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await api.saveRegion(id, {
        ...f,
        early_access_org_ids: early.split(/[\s,]+/).filter(Boolean),
      });
      await qc.invalidateQueries({ queryKey: ["admin", "regions"] });
      await qc.invalidateQueries({ queryKey: ["regions"] });
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const targetLabel = {
    storage_target_id: "Backup target",
    copy_target_id: "Copy target",
    files_target_id: "Files target",
  };
  const targetSelect = (
    k: "storage_target_id" | "copy_target_id" | "files_target_id",
    none: string,
  ) => (
    <Field label={targetLabel[k]}>
      {(fid) => (
        <Select
          id={fid}
          value={f[k] ?? ""}
          onChange={(e) => set(k, e.target.value || null)}
        >
          <option value="">{none}</option>
          {targets.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
            </option>
          ))}
        </Select>
      )}
    </Field>
  );
  return (
    <SidePanel
      open={!!region}
      onOpenChange={(o) => !o && onClose()}
      title={r ? `Edit ${r.name}` : "Add a region"}
      description="The control plane stays where it is; a region groups nodes, a pooler pair and storage."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            form="region-form"
            variant="primary"
            busy={busy}
          >
            Save
          </Button>
        </>
      }
    >
      <form id="region-form" className="flex flex-col gap-3" onSubmit={submit}>
        <Field label="ID">
          {(fid) => (
            <Input
              id={fid}
              required
              disabled={!!r}
              placeholder="ng-lagos"
              value={id}
              onChange={(e) => setId(e.target.value)}
            />
          )}
        </Field>
        <Field label="Name">
          {(fid) => (
            <Input
              id={fid}
              required
              value={f.name}
              onChange={(e) => set("name", e.target.value)}
            />
          )}
        </Field>
        <Field label="Country (two letters)">
          {(fid) => (
            <Input
              id={fid}
              maxLength={2}
              placeholder="NG"
              value={f.country ?? ""}
              onChange={(e) => set("country", e.target.value)}
            />
          )}
        </Field>
        <Field label="Pooler hostname">
          {(fid) => (
            <Input
              id={fid}
              placeholder="db.ng.example.com"
              value={f.pooler_host ?? ""}
              onChange={(e) => set("pooler_host", e.target.value)}
            />
          )}
        </Field>
        <Field
          label="API domain"
          hint="Optional. Projects here serve their backend services at <ref>.<this domain>, from this region's edge, so API traffic stays in the region. Empty is the platform's API domain."
        >
          {(fid) => (
            <Input
              id={fid}
              placeholder="ng.api.example.com"
              value={f.api_domain ?? ""}
              onChange={(e) => set("api_domain", e.target.value)}
            />
          )}
        </Field>
        <Field label="Provider">
          {(fid) => (
            <Select
              id={fid}
              value={f.provider ?? "manual"}
              onChange={(e) => set("provider", e.target.value)}
            >
              <option value="manual">
                Manual (colocated or local provider)
              </option>
              <option value="hetzner">Hetzner</option>
            </Select>
          )}
        </Field>
        {f.provider === "hetzner" && (
          <Field label="Location">
            {(fid) => (
              <Input
                id={fid}
                placeholder="nbg1"
                value={f.location ?? ""}
                onChange={(e) => set("location", e.target.value)}
              />
            )}
          </Field>
        )}
        <Field label="Pooler floating IP ID">
          {(fid) => (
            <Input
              id={fid}
              value={f.floating_ip_id ?? ""}
              onChange={(e) => set("floating_ip_id", e.target.value || null)}
            />
          )}
        </Field>
        {targetSelect("storage_target_id", "Platform default")}
        {targetSelect("copy_target_id", "No cross-region copies")}
        {targetSelect("files_target_id", "The backup target (not recommended)")}
        <p className="text-xs text-muted">
          Every edge holds the files target's credentials: give files a bucket,
          or credentials limited to files/, that can't reach the backups. Copy
          the files/ prefix there first; files already stored don't move
          (docs/regions.md).
        </p>
        <label className="flex items-start gap-2 text-[13px]">
          <input
            type="checkbox"
            aria-label="Offer data residency"
            checked={!!f.residency}
            onChange={(e) => set("residency", e.target.checked)}
          />
          <span>
            Offer data residency: projects here can keep their data, backups and
            branches in the country. Needs an in-country backup target.
          </span>
        </label>
        {r?.status === "hidden" && <Readiness id={r.id} />}
        {r && <RegionOverview id={r.id} />}
        <label className="flex items-start gap-2 text-[13px]">
          <input
            type="checkbox"
            aria-label="Hidden"
            checked={!!f.hidden}
            onChange={(e) => set("hidden", e.target.checked)}
          />
          <span>
            Hidden: no new projects except from early-access organisations
            (existing ones keep running).
          </span>
        </label>
        <Field
          label="Early-access organisations"
          hint="Organisation IDs, one per line, that may create projects here while the region is hidden: your own for a rehearsal, or a first customer."
        >
          {(fid) => (
            <textarea
              id={fid}
              rows={3}
              className="h-20 rounded-md border border-line-strong bg-surface-2 p-2 font-mono text-xs focus:border-accent focus:outline-none"
              value={early}
              onChange={(e) => setEarly(e.target.value)}
            />
          )}
        </Field>
        {err && <Alert>{err}</Alert>}
      </form>
    </SidePanel>
  );
}

/** A region's pooler pair, etcd cluster, and the HA projects still on
 * another region's etcd, with Move all (V4.1 §8.1). */
function RegionOverview({ id }: { id: string }) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["region-overview", id],
    queryFn: () => api.regionOverview(id),
    refetchInterval: (query) =>
      query.state.data?.move_all &&
      ["queued", "running"].includes(query.state.data.move_all.status)
        ? 3_000
        : false,
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (q.isPending) return null;
  if (q.isError) return <Alert>{errorMessage(q.error)}</Alert>;
  const o = q.data;
  const moving =
    !!o.move_all && ["queued", "running"].includes(o.move_all.status);
  const moveAll = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.moveAllToRegionEtcd(id);
      await qc.invalidateQueries({ queryKey: ["region-overview", id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="space-y-3 text-[13px]" data-testid="region-overview">
      <div>
        <div className="font-medium">Pooler pair</div>
        {o.pooler_hosts.length === 0 ? (
          <p className="text-muted">No pooler hosts in this region.</p>
        ) : (
          <ul>
            {o.pooler_hosts.map((h) => (
              <li key={h.id}>
                {h.name} · {h.failure_domain ?? "no failure domain"} ·{" "}
                {h.status}
              </li>
            ))}
          </ul>
        )}
        {o.pooler_pair_problem && (
          <Alert tone="warn">{o.pooler_pair_problem}</Alert>
        )}
      </div>
      <div>
        <div className="font-medium">
          etcd{" "}
          {o.etcd_ready ? (
            <Badge tone="ok">ready</Badge>
          ) : (
            <Badge tone="warn">not ready</Badge>
          )}
        </div>
        <p className="text-muted">
          {o.etcd_members.length} members,{" "}
          {o.etcd_members.filter((m) => m.status === "healthy").length} healthy.{" "}
          <Link to="/nodes" className="underline">
            Manage it on Nodes
          </Link>
          .
        </p>
        {o.etcd_problem && <p className="text-muted">{o.etcd_problem}</p>}
      </div>
      <div data-testid="ha-elsewhere">
        <div className="font-medium">
          HA projects on another region&apos;s etcd
        </div>
        {o.ha_elsewhere.length === 0 ? (
          <p className="text-muted">
            None: every HA project here uses this region&apos;s cluster.
          </p>
        ) : (
          <ul>
            {o.ha_elsewhere.map((p) => (
              <li key={p.project_id}>
                {p.name} · on {p.etcd_region}
              </li>
            ))}
          </ul>
        )}
        {o.ha_elsewhere.length > 0 && (
          <div className="mt-2">
            <Button
              busy={busy}
              disabled={!o.etcd_ready || moving}
              onClick={() => void moveAll()}
              data-testid="move-all"
            >
              Move all
            </Button>
            <p className="mt-1 text-muted">
              One project at a time; each pauses its own writes for a few
              seconds while its primary restarts.
            </p>
          </div>
        )}
        {o.move_all && (
          <p className="mt-1" data-testid="move-all-status">
            Move all: {o.move_all.status}
            {o.move_all.error ? ` (${o.move_all.error})` : ""}
          </p>
        )}
        {err && <Alert>{err}</Alert>}
      </div>
    </div>
  );
}

/** A hidden region's launch checks (V4.1 §13): it opens only when none of
 * the blocking ones fail. */
function Readiness({ id }: { id: string }) {
  const q = useQuery({
    queryKey: ["region-readiness", id],
    queryFn: () => api.regionReadiness(id),
  });
  if (q.isPending) return null;
  if (q.isError) return <Alert>{errorMessage(q.error)}</Alert>;
  return (
    <div className="space-y-1 text-[13px]" data-testid="region-readiness">
      <div className="font-medium">
        Launch checks{" "}
        {q.data.ready ? (
          <Badge tone="ok">Ready to open</Badge>
        ) : (
          <Badge tone="warn">Not ready</Badge>
        )}
      </div>
      <ul className="space-y-1">
        {q.data.checks.map((c) => (
          <li key={c.name} className="flex gap-2">
            <span
              className={
                c.ok
                  ? "text-ok-text"
                  : c.blocking
                    ? "text-danger-text"
                    : "text-warn-text"
              }
            >
              {c.ok ? "✓" : c.blocking ? "✕" : "!"}
            </span>
            <span>
              <strong>{c.name}</strong>: {c.detail}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
