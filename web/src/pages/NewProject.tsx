import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api, errorMessage, type ProjectCredentials } from "../api/client";
import { ProvisionProgress } from "../components/ProvisionProgress";
import { Link } from "@tanstack/react-router";
import {
  Alert,
  Button,
  Field,
  FormRow,
  Input,
  Page,
  Panel,
  Select,
  cx,
} from "../components/ui";
import { useCurrentOrg } from "../lib/org";
import { sessionQuery } from "../lib/session";

type Tier = "shared" | "dedicated";

export function NewProjectPage() {
  const qc = useQueryClient();
  const { org } = useCurrentOrg();
  const { data: session } = useQuery(sessionQuery);
  // Dedicated instances are placed on nodes only the platform admin manages;
  // others create them within their organisation's allowance, or ask
  // through a promotion (V2 §10.6).
  const platformAdmin = session?.user?.platform_role === "platform_admin";
  const quotas = useQuery({
    queryKey: ["org", org?.id, "quotas"],
    queryFn: () => api.orgQuotas(org!.id),
    enabled: !!org,
  });
  const allowance = quotas.data?.dedicated_allowance;
  const dedicatedAllowed =
    !!allowance &&
    (!!allowance.unlimited ||
      (quotas.data?.dedicated_use.instances ?? 0) < allowance.instances);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [tier, setTier] = useState<Tier>("shared");
  const [nodeId, setNodeId] = useState("");
  const [profile, setProfile] = useState("");
  const [volume, setVolume] = useState("");
  const [version, setVersion] = useState("");
  const [region, setRegion] = useState("");
  const [residency, setResidency] = useState(false);
  const regions = useQuery({ queryKey: ["regions"], queryFn: api.regions });
  // A hidden region is listed for its early-access organisations.
  const regionList = (regions.data?.items ?? []).filter(
    (r) =>
      !r.early_access_org_ids ||
      (!!org && r.early_access_org_ids.includes(org.id)),
  );
  const picked =
    regionList.find((r) => r.id === region) ?? regionList.find((r) => r.home);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [creds, setCreds] = useState<ProjectCredentials | null>(null);
  const profiles = useQuery({ queryKey: ["profiles"], queryFn: api.profiles });
  // Majors open for new projects, and any in preview (V4.1 §6.1).
  const previews = (profiles.data?.pg_version_lifecycle ?? [])
    .filter((v) => v.status === "preview")
    .map((v) => v.major);
  const versions = [...(profiles.data?.pg_versions ?? []), ...previews].sort(
    (a, b) => a - b,
  );
  const defaultVersion = profiles.data?.default_pg_version ?? 18;
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: api.nodes,
    enabled: tier === "dedicated" && platformAdmin,
  });
  const dedicatedNodes = (nodes.data?.items ?? []).filter(
    (n) => (n.role === "dedicated" || n.role === "both") && n.agent.registered,
  );

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      setCreds(
        await api.createProject({
          org_id: org?.id,
          name,
          description: description || undefined,
          tier,
          pg_version: version ? Number(version) : undefined,
          preview: previews.includes(Number(version)) || undefined,
          region: region || undefined,
          data_residency: residency && picked?.residency ? true : undefined,
          ...(tier === "dedicated"
            ? {
                node_id: nodeId || undefined,
                profile: profile || undefined,
                volume_gb: volume ? Number(volume) : undefined,
              }
            : {}),
        }),
      );
      void qc.invalidateQueries({ queryKey: ["projects"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (creds) return <ProvisionProgress creds={creds} />;

  const tierOption = (t: Tier, title: string, body: string) => (
    <label
      className={cx(
        "flex flex-1 cursor-pointer flex-col gap-1 rounded-md border px-3 py-2.5 text-sm",
        tier === t
          ? "border-accent bg-accent-soft"
          : "border-line-strong hover:bg-surface-2",
      )}
    >
      <span className="flex items-center gap-2 font-medium">
        <input
          type="radio"
          name="tier"
          checked={tier === t}
          onChange={() => setTier(t)}
        />
        {title}
      </span>
      <span className="text-xs text-muted">{body}</span>
    </label>
  );

  return (
    <Page
      title="Create a new project"
      description={`A PostgreSQL ${version || defaultVersion} database with its own role and connection strings.`}
      className="mx-auto w-full max-w-3xl"
    >
      <form onSubmit={submit}>
        <Panel
          footer={
            <>
              <Link to="/projects">
                <Button>Cancel</Button>
              </Link>
              <Button type="submit" variant="primary" busy={busy}>
                Create project
              </Button>
            </>
          }
        >
          <FormRow label="Organisation">
            <span className="text-[13px]">{org?.name ?? "—"}</span>
          </FormRow>
          <FormRow
            label="Project name"
            description="A label for you. The database itself gets a random name (p_…) so other tenants can’t learn project names."
            htmlFor="np-name"
          >
            <Input
              id="np-name"
              aria-label="Name"
              required
              maxLength={64}
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoFocus
            />
          </FormRow>
          <FormRow
            label="Description"
            description="Optional."
            htmlFor="np-desc"
          >
            <Input
              id="np-desc"
              aria-label="Description (optional)"
              maxLength={1000}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </FormRow>
          <FormRow
            label="Tier"
            description="You can promote a shared project to dedicated, or move it back, later."
          >
            <div
              className="flex flex-col gap-2 sm:flex-row"
              role="radiogroup"
              aria-label="Tier"
            >
              {tierOption(
                "shared",
                "Shared",
                "A database on the shared cluster. Ready in a second; nightly logical backups.",
              )}
              {dedicatedAllowed &&
                tierOption(
                  "dedicated",
                  "Dedicated",
                  "Its own Postgres container with CPU and memory limits; continuous backups and point-in-time restore.",
                )}
            </div>
          </FormRow>
          {regionList.length > 1 && (
            <FormRow
              label="Region"
              description="Where the database, its backups and its branches run. Moving regions later is a zero-downtime move that changes the hostname."
              htmlFor="np-region"
            >
              <Select
                id="np-region"
                aria-label="Region"
                value={region}
                onChange={(e) => {
                  setRegion(e.target.value);
                  setResidency(false);
                }}
              >
                {regionList.map((r) => (
                  <option key={r.id} value={r.home ? "" : r.id}>
                    {r.name}
                    {r.country ? ` (${r.country})` : ""}
                    {r.home ? " (default)" : ""}
                    {r.early_access_org_ids ? " (early access)" : ""}
                  </option>
                ))}
              </Select>
              {picked?.residency && (
                <label className="mt-2 flex items-start gap-2 text-[13px]">
                  <input
                    type="checkbox"
                    aria-label="Data residency"
                    checked={residency}
                    onChange={(e) => setResidency(e.target.checked)}
                  />
                  <span>
                    Data must stay in {picked.country}: the database, its
                    backups and its branches stay in {picked.name}, with no
                    cross-region backup copies. Exports stay available.
                  </span>
                </label>
              )}
            </FormRow>
          )}
          {versions.length > 1 && (
            <FormRow
              label="Postgres version"
              description="Newer majors can be upgraded to later, with a pause of a few seconds."
              htmlFor="np-version"
            >
              <Select
                id="np-version"
                aria-label="Postgres version"
                value={version}
                onChange={(e) => setVersion(e.target.value)}
              >
                {[...versions].reverse().map((v) => (
                  <option key={v} value={v === defaultVersion ? "" : String(v)}>
                    Postgres {v}
                    {v === defaultVersion ? " (default)" : ""}
                    {previews.includes(v) ? " (preview)" : ""}
                  </option>
                ))}
              </Select>
            </FormRow>
          )}
          {tier === "dedicated" && (
            <FormRow
              label="Compute"
              description="Where it runs and how big it is."
            >
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                <Field label="Node">
                  {(id) => (
                    <Select
                      id={id}
                      value={nodeId}
                      onChange={(e) => setNodeId(e.target.value)}
                    >
                      <option value="">Least loaded</option>
                      {dedicatedNodes.map((n) => (
                        <option
                          key={n.id}
                          value={n.id}
                          disabled={n.status !== "healthy"}
                        >
                          {n.status === "healthy"
                            ? n.name
                            : `${n.name} (${n.status})`}
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
                <Field label="Size">
                  {(id) => (
                    <Select
                      id={id}
                      value={profile}
                      onChange={(e) => setProfile(e.target.value)}
                    >
                      {(profiles.data?.items ?? []).map((p) => (
                        <option
                          key={p.name}
                          value={
                            p.name === profiles.data?.default_profile
                              ? ""
                              : p.name
                          }
                        >
                          {p.name}: {p.cpus} CPU, {p.memory_mb / 1024} GB
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
                <Field label="Volume (GB)">
                  {(id) => (
                    <Input
                      id={id}
                      type="number"
                      min={1}
                      max={16384}
                      value={volume}
                      placeholder={String(
                        profiles.data?.default_volume_gb ?? 20,
                      )}
                      onChange={(e) => setVolume(e.target.value)}
                    />
                  )}
                </Field>
              </div>
              {nodes.data && dedicatedNodes.length === 0 && (
                <Alert tone="warn">
                  No node with an agent accepts dedicated instances yet. On the
                  Nodes page, give a node the role "dedicated" or "both", or add
                  one.
                </Alert>
              )}
              {nodes.data &&
                dedicatedNodes.length > 0 &&
                !dedicatedNodes.some((n) => n.status === "healthy") && (
                  <Alert tone="warn">
                    Every node that takes dedicated instances is unreachable
                    right now, so a new one cannot be placed. Check the agents
                    on the Nodes page.
                  </Alert>
                )}
            </FormRow>
          )}
          {err && <Alert>{err}</Alert>}
        </Panel>
      </form>
    </Page>
  );
}
