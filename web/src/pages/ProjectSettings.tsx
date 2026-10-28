import { useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import {
  api,
  errorMessage,
  type Project,
  type ProjectCredentials,
} from "../api/client";
import { ConfirmDestroy } from "../components/ConfirmDelete";
import { CredentialPanel } from "../components/Credentials";
import { DemoteCard } from "../components/DemoteCard";
import { PromoteCard } from "../components/PromoteCard";
import { HACard } from "../components/HACard";
import { ReplicasCard } from "../components/ReplicasCard";
import { MovesCard, UpgradeCard } from "../components/UpgradeCard";
import { ProjectStorageCard } from "../components/StorageTargets";
import { StorageCard, SwitchCredentialsCard } from "../components/TenancyCards";
import { useOperationToast } from "../components/Toasts";
import { useStepUp } from "../components/StepUp";
import {
  Alert,
  Badge,
  Button,
  FormRow,
  Input,
  KeyValues,
  Page,
  Panel,
  Section,
  Select,
  Switch,
} from "../components/ui";
import { formatBytes, parseBytes } from "../lib/format";
import { setCurrentOrg, useCurrentOrg } from "../lib/org";
import { useOperationStream } from "../lib/useOperationStream";
import { InstancePanel } from "./ProjectOverview";
import { ResizeCard } from "../components/ResizeCard";
import { VersionBanner } from "../components/VersionBanner";
import { useProject } from "./ProjectOverview";

// Project Settings (docs/ui-redesign.md, phase 4), in Studio's layout:
// General, Database, Compute and tier, and Backup storage, each a page of
// panels with settings rows.

/** Project Settings → General: name, data, moving it, and deleting it. */
export function ProjectSettingsPage() {
  const { data: p } = useProject();
  if (!p) return null;
  return (
    <Page
      title="Project Settings"
      description="General settings for this project."
      testId="settings-general"
    >
      <GeneralPanel p={p} />
      <RegionPanel p={p} />
      <DataPanel p={p} />
      <TransferPanel p={p} />
      <DangerPanel p={p} />
    </Page>
  );
}

/** Project Settings → Database: guardrails, the password, credentials and
 * the storage the project uses. */
export function ProjectDatabaseSettingsPage() {
  const { data: p } = useProject();
  if (!p) return null;
  return (
    <Page
      title="Database Settings"
      description="Connections, timeouts, the console and the database password."
      testId="settings-database"
    >
      <GuardrailsPanel p={p} />
      <RotatePanel p={p} />
      <SwitchCredentialsCard p={p} />
      <StorageCard p={p} />
    </Page>
  );
}

/** Project Settings → Compute and tier: the instance, and the promote and
 * demote wizards. */
export function ProjectComputePage() {
  const { data: p } = useProject();
  if (!p) return null;
  return (
    <Page
      title="Compute and tier"
      description="Where the database runs, and moving it between the shared and dedicated tiers."
      testId="settings-compute"
    >
      <VersionBanner p={p} />
      <Panel title="Tier">
        <KeyValues
          items={[
            [
              "Tier",
              <Badge key="t" tone={p.tier === "dedicated" ? "accent" : "muted"}>
                {p.tier === "dedicated" ? "Dedicated" : "Shared"}
              </Badge>,
            ],
            [
              "What that means",
              p.tier === "dedicated"
                ? "Its own Postgres instance, with continuous WAL archiving and point-in-time recovery."
                : "A database on a shared cluster, with nightly backups and guardrails on connections and storage.",
            ],
          ]}
        />
      </Panel>
      {p.tier === "dedicated" && p.instance && (
        <InstancePanel projectId={p.id} instance={p.instance} />
      )}
      {p.tier === "dedicated" && <ManageRolesPanel p={p} />}
      <ResizeCard p={p} />
      <HACard p={p} />
      <ReplicasCard p={p} />
      <UpgradeCard p={p} />
      {!p.parent_project_id && <PromoteCard p={p} />}
      <DemoteCard p={p} />
      <MovesCard p={p} />
    </Page>
  );
}

/** Project Settings → Backup storage: where backups go, and the project's
 * own key. */
export function ProjectStorageSettingsPage() {
  const { data: p } = useProject();
  if (!p) return null;
  return (
    <Page
      title="Backup storage"
      description="Where this project's backups are stored and the key they are encrypted with."
      testId="settings-storage"
    >
      <ProjectStorageCard p={p} canManage={p.my_role === "admin"} />
    </Page>
  );
}

function GeneralPanel({ p }: { p: Project }) {
  const qc = useQueryClient();
  const [name, setName] = useState(p.name);
  const [description, setDescription] = useState(p.description ?? "");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const dirty = name !== p.name || description !== (p.description ?? "");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      await api.updateProject(p.id, { name, description });
      await qc.invalidateQueries({ queryKey: ["project", p.id] });
      setMsg({ ok: true, text: "Saved." });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit}>
      <Panel
        title="General"
        footer={
          <>
            {msg && (
              <span
                className={
                  msg.ok
                    ? "mr-auto text-[12px] text-ok-text"
                    : "mr-auto text-[12px] text-danger-text"
                }
              >
                {msg.text}
              </span>
            )}
            <Button
              disabled={!dirty}
              onClick={() => {
                setName(p.name);
                setDescription(p.description ?? "");
              }}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              busy={busy}
              disabled={!dirty}
            >
              Save
            </Button>
          </>
        }
      >
        <FormRow
          label="Project name"
          description="Renaming keeps the database name and connection strings."
          htmlFor="project-name"
        >
          <Input
            id="project-name"
            aria-label="Name"
            required
            maxLength={64}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </FormRow>
        <FormRow label="Description" htmlFor="project-description">
          <Input
            id="project-description"
            aria-label="Description"
            maxLength={1000}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </FormRow>
        <FormRow label="Database">
          <code className="font-mono text-[13px]">{p.db_name}</code>
        </FormRow>
      </Panel>
    </form>
  );
}

/** Role management on a dedicated project: the owner role's CREATEROLE,
 * for applications whose migrations create roles, and the logins it
 * created that the poolers accept. */
function ManageRolesPanel({ p }: { p: Project }) {
  const qc = useQueryClient();
  const { org } = useCurrentOrg();
  const owner = org?.role === "owner";
  const key = ["project", p.id, "manage-roles"];
  const mr = useQuery({ queryKey: key, queryFn: () => api.manageRoles(p.id) });
  const stepUp = useStepUp(
    "Role management lets this project's owner create database roles: confirm with your password.",
  );
  const [err, setErr] = useState<string | null>(null);
  const [skipped, setSkipped] = useState<{ name: string; reason: string }[]>(
    [],
  );
  const toggle = async (on: boolean) => {
    setErr(null);
    try {
      await stepUp.run(async () => {
        await api.setManageRoles(p.id, on);
        await qc.invalidateQueries({ queryKey: key });
        await qc.invalidateQueries({ queryKey: ["project", p.id] });
      });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  const sync = async () => {
    setErr(null);
    try {
      const res = await api.syncManageRoles(p.id);
      setSkipped(res.skipped ?? []);
      await qc.invalidateQueries({ queryKey: key });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  const logins = mr.data?.logins ?? [];
  return (
    <Panel title="Database roles">
      <FormRow
        label="Let the owner create roles"
        description={
          owner
            ? `Gives the owner role CREATEROLE, for migrations that create their own roles. It can manage only the roles it creates. Logins named ${p.db_name}_<name> connect through the poolers within 30 seconds.`
            : "Only organisation owners can change this."
        }
      >
        <Switch
          checked={!!mr.data?.enabled}
          disabled={!owner || mr.isLoading}
          onCheckedChange={(v) => void toggle(v)}
          aria-label="Let the owner create roles"
          data-testid="manage-roles"
        />
      </FormRow>
      {(mr.data?.enabled || logins.length > 0) && (
        <FormRow
          label="Logins the poolers accept"
          description="Created by the owner. Each gets the project's connection limit."
        >
          <div className="flex flex-col gap-2 text-[13px]">
            {logins.length === 0 ? (
              <span className="text-fg-muted">None yet.</span>
            ) : (
              <ul className="font-mono">
                {logins.map((l) => (
                  <li key={l.name}>{l.name}</li>
                ))}
              </ul>
            )}
            {skipped.length > 0 && (
              <ul className="text-fg-muted">
                {skipped.map((s) => (
                  <li key={s.name}>
                    <span className="font-mono">{s.name}</span>: not synced (
                    {s.reason})
                  </li>
                ))}
              </ul>
            )}
            <div>
              <Button onClick={() => void sync()}>Sync now</Button>
            </div>
          </div>
        </FormRow>
      )}
      {err && <Alert>{err}</Alert>}
      {stepUp.dialog}
    </Panel>
  );
}

/** "Contains sensitive data" and, for a branch, nightly backups (V2 §8.4, §8.5). */
/** The project's region and its data residency setting (V3 §6.3):
 * organisation owners turn residency on or off, after a step-up. */
function RegionPanel({ p }: { p: Project }) {
  const qc = useQueryClient();
  const { org } = useCurrentOrg();
  const owner = org?.role === "owner";
  const regions = useQuery({ queryKey: ["regions"], queryFn: api.regions });
  const region = regions.data?.items.find((r) => r.id === p.region);
  const stepUp = useStepUp(
    "Data residency changes where this project's data may go: confirm with your password and a code.",
  );
  const [err, setErr] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const toggle = async (on: boolean) => {
    setErr(null);
    setNote(null);
    try {
      await stepUp.run(async () => {
        const res = await api.setResidency(p.id, on);
        if (res.removed_copies > 0)
          setNote(
            `${res.removed_copies} backup cop${res.removed_copies === 1 ? "y" : "ies"} outside ${region?.name ?? p.region} deleted.`,
          );
        await qc.invalidateQueries({ queryKey: ["project", p.id] });
      });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  return (
    <Panel title="Region">
      <FormRow
        label="Region"
        description="Where the database, its backups and its branches run."
      >
        <span className="text-[13px]" data-testid="project-region">
          {region
            ? `${region.name}${region.country ? ` (${region.country})` : ""}`
            : p.region}
        </span>
      </FormRow>
      {p.forward_region &&
        p.forward_until &&
        new Date(p.forward_until) > new Date() && (
          <Alert tone="accent">
            Moved from {p.forward_region}: the old hostname keeps working until{" "}
            {new Date(p.forward_until).toLocaleDateString()}. Update your
            connection strings before then.
          </Alert>
        )}
      {(region?.residency || p.data_residency) && (
        <FormRow
          label={`Data must stay in ${region?.country || p.region}`}
          description={
            owner
              ? "The database, its backups and its branches stay in this region, with no cross-region backup copies. Exports stay available."
              : "Only organisation owners can change this."
          }
        >
          <Switch
            checked={!!p.data_residency}
            disabled={!owner}
            onCheckedChange={(v) => void toggle(v)}
            aria-label="Data residency"
            data-testid="data-residency"
          />
        </FormRow>
      )}
      {note && <Alert tone="accent">{note}</Alert>}
      {err && <Alert>{err}</Alert>}
      {stepUp.dialog}
    </Panel>
  );
}

function DataPanel({ p }: { p: Project }) {
  const qc = useQueryClient();
  const [err, setErr] = useState<string | null>(null);
  const save = async (b: Parameters<typeof api.updateProject>[1]) => {
    setErr(null);
    try {
      await api.updateProject(p.id, b);
      await qc.invalidateQueries({ queryKey: ["project", p.id] });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  return (
    <Panel title="Data">
      <FormRow
        label="Contains sensitive data"
        description="Branches copy the schema only unless a project admin asks for the data."
      >
        <Switch
          checked={!!p.sensitive_data}
          onCheckedChange={(v) => void save({ sensitive_data: v })}
          aria-label="Contains sensitive data"
          data-testid="sensitive-data"
        />
      </FormRow>
      {p.parent_project_id && (
        <FormRow
          label="Back this branch up nightly"
          description="Off by default: branches are disposable."
        >
          <Switch
            checked={!!p.branch?.backups}
            onCheckedChange={(v) => void save({ branch_backups: v })}
            aria-label="Back this branch up nightly"
          />
        </FormRow>
      )}
      {err && <Alert>{err}</Alert>}
    </Panel>
  );
}

function GuardrailsPanel({ p }: { p: Project }) {
  const qc = useQueryClient();
  const toast = useOperationToast();
  const s = p.settings;
  const initial = () => ({
    connection_limit: String(s.connection_limit),
    pool_size: String(s.pool_size),
    statement_timeout: s.statement_timeout,
    idle: s.idle_in_transaction_session_timeout,
    disk: formatBytes(s.disk_warn_bytes),
    readOnly: s.console_read_only,
  });
  const [form, setForm] = useState(initial);
  useEffect(() => {
    setForm(initial());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    s.connection_limit,
    s.pool_size,
    s.statement_timeout,
    s.idle_in_transaction_session_timeout,
    s.disk_warn_bytes,
    s.console_read_only,
  ]);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const disk = parseBytes(form.disk);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      const res = await api.updateProject(p.id, {
        settings: {
          connection_limit: Number(form.connection_limit),
          pool_size: Number(form.pool_size),
          statement_timeout: form.statement_timeout.trim(),
          idle_in_transaction_session_timeout: form.idle.trim(),
          disk_warn_bytes: disk ?? undefined,
          console_read_only: form.readOnly,
        },
      });
      if (res.operation) toast(res.operation.id, `Apply settings · ${p.name}`);
      await qc.invalidateQueries({ queryKey: ["project", p.id] });
      setMsg({
        ok: true,
        text: res.operation
          ? "Saved; applying to the database and pooler."
          : "Saved.",
      });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  const set =
    (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
      setForm((f) => ({ ...f, [k]: e.target.value }));

  return (
    <form onSubmit={submit}>
      <Panel
        title="Connections and timeouts"
        description="Applied to the database and the pooler; existing connections keep their settings until they reconnect."
        footer={
          <>
            {msg && (
              <span
                className={
                  msg.ok
                    ? "mr-auto text-[12px] text-ok-text"
                    : "mr-auto text-[12px] text-danger-text"
                }
                role="status"
              >
                {msg.text}
              </span>
            )}
            <Button onClick={() => setForm(initial())}>Cancel</Button>
            <Button
              type="submit"
              variant="primary"
              busy={busy}
              disabled={disk === null}
            >
              Save guardrails
            </Button>
          </>
        }
      >
        <FormRow
          label="Max backend connections"
          description="Connections Postgres accepts for this database."
          htmlFor="g-conn"
        >
          <Input
            id="g-conn"
            aria-label="Max backend connections"
            type="number"
            min={1}
            max={1000}
            value={form.connection_limit}
            onChange={set("connection_limit")}
          />
        </FormRow>
        <FormRow
          label="Pooler pool size"
          description="Server connections the pooler keeps for transaction mode."
          htmlFor="g-pool"
        >
          <Input
            id="g-pool"
            aria-label="Pooler pool size"
            type="number"
            min={1}
            max={1000}
            value={form.pool_size}
            onChange={set("pool_size")}
          />
        </FormRow>
        <FormRow
          label="Statement timeout"
          description="e.g. 60s, 500ms, 5min; empty to unset."
          htmlFor="g-stmt"
        >
          <Input
            id="g-stmt"
            aria-label="Statement timeout"
            value={form.statement_timeout}
            onChange={set("statement_timeout")}
            className="font-mono"
          />
        </FormRow>
        <FormRow label="Idle in transaction timeout" htmlFor="g-idle">
          <Input
            id="g-idle"
            aria-label="Idle in transaction timeout"
            value={form.idle}
            onChange={set("idle")}
            className="font-mono"
          />
        </FormRow>
        <FormRow
          label="Disk warning"
          description="Alert when the database grows past this size."
          htmlFor="g-disk"
        >
          <Input
            id="g-disk"
            aria-label="Disk warning"
            value={form.disk}
            onChange={set("disk")}
          />
          {disk === null && (
            <span className="text-[12px] text-danger-text">
              Use a size like 1 GiB or 500 MB.
            </span>
          )}
        </FormRow>
        <FormRow
          label="SQL console is read-only"
          description="Queries run one statement at a time inside a read-only transaction."
        >
          <Switch
            checked={form.readOnly}
            onCheckedChange={(v) => setForm((f) => ({ ...f, readOnly: v }))}
            aria-label="SQL console is read-only"
          />
        </FormRow>
      </Panel>
    </form>
  );
}

function RotatePanel({ p }: { p: Project }) {
  const [creds, setCreds] = useState<ProjectCredentials | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const stream = useOperationStream(creds?.operation.id);
  const rotate = async () => {
    setBusy(true);
    setErr(null);
    try {
      setCreds(await api.rotatePassword(p.id));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel title="Database password">
      {!creds ? (
        <FormRow
          label="Reset the database password"
          description="Issues a new password. The old one stops working as soon as the rotation finishes."
        >
          <div>
            <Button
              onClick={rotate}
              busy={busy}
              disabled={p.status !== "active"}
            >
              Rotate password
            </Button>
          </div>
          {err && <Alert>{err}</Alert>}
        </FormRow>
      ) : (
        <div className="flex flex-col gap-3">
          {stream.status === "failed" && (
            <Alert title="Rotation failed">
              {stream.error}. The old password still works.
            </Alert>
          )}
          <CredentialPanel
            creds={creds}
            ready={stream.status === "succeeded"}
            onDismiss={() => setCreds(null)}
          />
        </div>
      )}
    </Panel>
  );
}

function DangerPanel({ p }: { p: Project }) {
  const [open, setOpen] = useState(false);
  const [finalBackup, setFinalBackup] = useState(true);
  const toast = useOperationToast();
  const navigate = useNavigate();
  const qc = useQueryClient();
  return (
    <Section title="Danger zone">
      <Panel tone="danger">
        <FormRow
          label="Delete project"
          description="Drops the database and its role and removes the connection strings. A final backup is kept for 30 days."
        >
          <div>
            <Button variant="danger" onClick={() => setOpen(true)}>
              Delete project
            </Button>
          </div>
        </FormRow>
      </Panel>
      <ConfirmDestroy
        open={open}
        onClose={() => setOpen(false)}
        title={`Delete ${p.name}`}
        name={p.name}
        description="Confirm with the project name, your password, and an authenticator code."
        action="Delete project"
        run={async () => {
          const op = await api.deleteProject(p.id, p.name, !finalBackup);
          toast(op.id, `Delete · ${p.name}`);
          await qc.invalidateQueries({ queryKey: ["projects"] });
          await navigate({ to: "/projects" });
        }}
      >
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={finalBackup}
            onChange={(e) => setFinalBackup(e.target.checked)}
          />
          Take a final backup first (kept 30 days)
        </label>
      </ConfirmDestroy>
    </Section>
  );
}

/** Moves the project to another organisation the user owns (V2 §2.1). */
function TransferPanel({ p }: { p: Project }) {
  const { orgs } = useCurrentOrg();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const here = orgs.find((o) => o.id === p.org_id);
  const targets = orgs.filter((o) => o.id !== p.org_id && o.role === "owner");
  const [to, setTo] = useState("");
  const [open, setOpen] = useState(false);
  if (here?.role !== "owner" || targets.length === 0) return null;
  const target = targets.find((o) => o.id === to);
  return (
    <Panel title="Move to another organisation">
      <FormRow
        label="Destination"
        description="The database, its URL, and its members' logins stay as they are; members join the new organisation as members."
      >
        <div className="flex flex-wrap gap-2">
          <Select
            aria-label="Destination organisation"
            value={to}
            onChange={(e) => setTo(e.target.value)}
          >
            <option value="">Choose an organisation…</option>
            {targets.map((o) => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </Select>
          <Button disabled={!to} onClick={() => setOpen(true)}>
            Move
          </Button>
        </div>
      </FormRow>
      <ConfirmDestroy
        open={open}
        onClose={() => setOpen(false)}
        title={`Move ${p.name}`}
        name={p.name}
        description={`Moves the project into ${target?.name ?? "the chosen organisation"}.`}
        action="Move project"
        run={async () => {
          await api.transferProject(p.id, to);
          setCurrentOrg(to);
          await qc.invalidateQueries();
          await navigate({ to: "/projects/$id", params: { id: p.id } });
        }}
      />
    </Panel>
  );
}
