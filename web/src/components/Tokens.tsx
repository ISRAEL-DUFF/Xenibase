import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage, type APIToken, type CreatedToken, type TokenScope } from "../api/client";
import { formatDate, relativeTime } from "../lib/format";
import { Alert, Badge, Button, Panel, CodeBlock, CopyField, Field, Input, Select, SidePanel, Table } from "./ui";

export const SCOPE_TEXT: Record<TokenScope, string> = {
  read: "View everything you can see in the organisation, and run read-only SQL",
  write: "Create and change: run SQL that writes, take backups, create projects",
  admin: "Destructive and settings actions: delete, promote, restore in place, members",
};

const EXPIRIES = [7, 30, 90, 180, 365];

function statusTone(s: APIToken["status"]) {
  return s === "active" ? "ok" : s === "expired" ? "warn" : "muted";
}

/** Scope checkboxes: read is always on, admin needs write (V2 §7.2). */
export function ScopePicker({ value, onChange, allowed }: { value: TokenScope[]; onChange: (v: TokenScope[]) => void; allowed?: TokenScope[] }) {
  const has = (s: TokenScope) => value.includes(s);
  const toggle = (s: TokenScope, on: boolean) => {
    let next = value.filter((x) => x !== s);
    if (on) next = [...next, s];
    if (s === "write" && !on) next = next.filter((x) => x !== "admin");
    if (s === "admin" && on && !next.includes("write")) next = [...next, "write"];
    if (!next.includes("read")) next = ["read", ...next];
    onChange((["read", "write", "admin"] as TokenScope[]).filter((x) => next.includes(x)));
  };
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="mb-1 text-sm font-medium">Scopes</legend>
      {(["read", "write", "admin"] as TokenScope[]).map((s) => (
        <label key={s} className="flex items-start gap-2 text-sm">
          <input
            type="checkbox"
            className="mt-1"
            checked={has(s)}
            disabled={s === "read" || (allowed && !allowed.includes(s))}
            onChange={(e) => toggle(s, e.target.checked)}
            data-testid={`scope-${s}`}
          />
          <span>
            <span className="font-mono">{s}</span>
            <span className="block text-xs text-muted">{SCOPE_TEXT[s]}</span>
          </span>
        </label>
      ))}
    </fieldset>
  );
}

/** Optional project restriction for a token, within one organisation; `projects` skips the lookup. */
export function ProjectPicker({
  org,
  value,
  onChange,
  projects,
}: {
  org: string;
  value: string[] | null;
  onChange: (v: string[] | null) => void;
  projects?: { id: string; name: string }[];
}) {
  const q = useQuery({ queryKey: ["projects", org, "for-token"], queryFn: () => api.projects(org), enabled: !!org && !projects });
  const items = projects ?? q.data?.items ?? [];
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="mb-1 text-sm font-medium">Projects</legend>
      <label className="flex items-center gap-2 text-sm">
        <input type="radio" checked={value === null} onChange={() => onChange(null)} data-testid="token-all-projects" />
        All the projects you can access in the organisation
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="radio" checked={value !== null} onChange={() => onChange([])} data-testid="token-some-projects" />
        Only these projects
      </label>
      {value !== null && (
        <div className="ml-6 flex max-h-40 flex-col gap-1 overflow-y-auto">
          {items.map((p) => (
            <label key={p.id} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={value.includes(p.id)}
                onChange={(e) => onChange(e.target.checked ? [...value, p.id] : value.filter((x) => x !== p.id))}
                data-testid={`token-project-${p.name}`}
              />
              {p.name}
            </label>
          ))}
          {items.length === 0 && <span className="text-xs text-muted">No projects in this organisation.</span>}
        </div>
      )}
    </fieldset>
  );
}

/** The secret, shown once, with how to use it. */
export function TokenSecret({ created }: { created: CreatedToken }) {
  return (
    <div className="flex flex-col gap-3" data-testid="token-created">
      <Alert tone="warn">Copy the token now: Xenibase shows it once and keeps only a hash.</Alert>
      <CopyField label={`Token “${created.token.name}”`} value={created.secret} secret testId="token-secret" />
      <p className="text-sm text-muted">Use it from CI or scripts:</p>
      <CodeBlock code={`export XENIBASE_SERVER=${window.location.origin}\nexport XENIBASE_TOKEN=<the token>\nxenibase projects list`} />
    </div>
  );
}

function NewTokenModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const orgs = useQuery({ queryKey: ["orgs"], queryFn: api.orgs });
  const [name, setName] = useState("");
  const [org, setOrg] = useState("");
  const [scopes, setScopes] = useState<TokenScope[]>(["read"]);
  const [projects, setProjects] = useState<string[] | null>(null);
  const [days, setDays] = useState(90);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  useEffect(() => {
    if (!org && orgs.data?.items.length) setOrg(orgs.data.items[0].id);
  }, [org, orgs.data]);
  const close = () => {
    setCreated(null);
    setName("");
    setScopes(["read"]);
    setProjects(null);
    setErr(null);
    onClose();
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      setCreated(await api.createToken({ name: name.trim(), org_id: org, scopes, project_ids: projects, expires_in_days: days }));
      await qc.invalidateQueries({ queryKey: ["me", "tokens"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SidePanel
      title="New API token"
      description="For the xenibase CLI, CI jobs and scripts."
      open={open}
      onOpenChange={(o) => !o && close()}
      footer={
        created ? (
          <Button variant="primary" onClick={close}>
            Done
          </Button>
        ) : (
          <>
            <Button onClick={close}>Cancel</Button>
            <Button
              type="submit"
              form="new-token-form"
              variant="primary"
              busy={busy}
              disabled={!name.trim() || !org || (projects !== null && projects.length === 0)}
              data-testid="create-token"
            >
              Create token
            </Button>
          </>
        )
      }
    >
      {created ? (
        <TokenSecret created={created} />
      ) : (
        <form id="new-token-form" className="flex flex-col gap-5" onSubmit={submit}>
          <Field label="Name" hint="What it's for, e.g. “GitHub Actions — blog”.">
            {(id) => <Input id={id} required maxLength={100} value={name} onChange={(e) => setName(e.target.value)} autoFocus />}
          </Field>
          <Field label="Organisation" hint="A token acts in one organisation.">
            {(id) => (
              <Select id={id} value={org} onChange={(e) => (setOrg(e.target.value), setProjects(null))}>
                {orgs.data?.items.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.name}
                    {o.personal ? " (personal)" : ""}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <ScopePicker value={scopes} onChange={setScopes} />
          {org && <ProjectPicker org={org} value={projects} onChange={setProjects} />}
          <Field label="Expires after">
            {(id) => (
              <Select id={id} value={days} onChange={(e) => setDays(Number(e.target.value))}>
                {EXPIRIES.map((d) => (
                  <option key={d} value={d}>
                    {d} days
                  </option>
                ))}
              </Select>
            )}
          </Field>
          {err && <Alert>{err}</Alert>}
        </form>
      )}
    </SidePanel>
  );
}

function TokenRows({ items, revoke, showOrg, showUser }: { items: APIToken[]; revoke: (t: APIToken) => Promise<void>; showOrg?: boolean; showUser?: boolean }) {
  return (
    <Table head={["Name", ...(showUser ? ["Owner"] : []), ...(showOrg ? ["Organisation"] : []), "Scopes", "Projects", "Expires", "Last used", ""]}>
      {items.map((t) => (
        <tr key={t.id} data-testid={`token-${t.name}`}>
          <td className="px-3 py-1.5 text-sm">
            {t.name}
            <div className="font-mono text-xs text-muted">{t.prefix}…</div>
            {t.oauth_client_name && <div className="text-xs text-muted">Connected app</div>}
          </td>
          {showUser && <td className="px-3 py-1.5 text-xs">{t.user_email}</td>}
          {showOrg && <td className="px-3 py-1.5 text-xs">{t.org_name}</td>}
          <td className="px-3 py-1.5 font-mono text-xs">{t.scopes.join(", ")}</td>
          <td className="px-3 py-1.5 text-xs">{t.project_ids ? `${t.project_ids.length} only` : "all"}</td>
          <td className="px-3 py-1.5 text-xs">{t.status === "active" ? formatDate(t.expires_at) : <Badge tone={statusTone(t.status)}>{t.status}</Badge>}</td>
          <td className="px-3 py-1.5 text-xs text-muted">
            {t.last_used_at ? `${relativeTime(t.last_used_at)}${t.last_used_ip ? ` from ${t.last_used_ip}` : ""}` : "never"}
          </td>
          <td className="px-3 py-1.5 text-right">
            {t.status === "active" && (
              <Button className="text-xs" onClick={() => void revoke(t)} data-testid={`revoke-token-${t.name}`}>
                Revoke
              </Button>
            )}
          </td>
        </tr>
      ))}
    </Table>
  );
}

/** Account → Tokens (V2 §7.2), and the apps connected through OAuth. */
export function TokensCard() {
  const q = useQuery({ queryKey: ["me", "tokens"], queryFn: api.myTokens });
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const revoke = async (t: APIToken, what = `Revoke “${t.name}”? Anything using it stops working at once.`) => {
    if (!window.confirm(what)) return;
    try {
      await api.revokeMyToken(t.id);
      await qc.invalidateQueries({ queryKey: ["me", "tokens"] });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  const own = q.data?.items.filter((t) => t.created_via !== "oauth") ?? [];
  const apps = q.data?.items.filter((t) => t.created_via === "oauth") ?? [];
  return (
    <>
      <Panel
        title="Your API tokens"
        actions={
          <Button onClick={() => setOpen(true)} data-testid="new-token">
            New token
          </Button>
        }
      >
        <div id="tokens" className="flex flex-col gap-3">
          <p className="text-sm text-muted">
            For the <span className="font-mono">xenibase</span> CLI, CI jobs and scripts. A token acts in one organisation, never with more than you can do, and
            stops when you leave it. <span className="font-mono">xenibase login</span> creates one for you.
          </p>
          {own.length > 0 && <TokenRows items={own} revoke={revoke} showOrg />}
          {err && <Alert>{err}</Alert>}
        </div>
        <NewTokenModal open={open} onClose={() => setOpen(false)} />
      </Panel>
      <ConnectedApps items={apps} revoke={revoke} />
    </>
  );
}

/** Tokens issued to third-party apps through OAuth: one row per connection. */
function ConnectedApps({ items, revoke }: { items: APIToken[]; revoke: (t: APIToken, what: string) => Promise<void> }) {
  return (
    <Panel title="Connected apps">
      <div id="connected-apps" className="flex flex-col gap-3">
        <p className="text-sm text-muted">
          Apps you connected to an organisation with your consent. Each holds a token with the scopes you granted, until it expires or you disconnect it;
          disconnecting breaks the app's connection at once.
        </p>
        {items.length === 0 ? (
          <p className="text-sm text-muted" data-testid="no-connected-apps">
            No apps are connected.
          </p>
        ) : (
          <Table head={["App", "Organisation", "Scopes", "Projects", "Expires", "Last used", ""]}>
            {items.map((t) => (
              <tr key={t.id} data-testid={`connected-app-${t.oauth_client_name ?? t.name}`}>
                <td className="px-3 py-1.5 text-sm">
                  {t.oauth_client_name ?? t.name}
                  <div className="font-mono text-xs text-muted">{t.prefix}…</div>
                </td>
                <td className="px-3 py-1.5 text-xs">{t.org_name}</td>
                <td className="px-3 py-1.5 font-mono text-xs">{t.scopes.join(", ")}</td>
                <td className="px-3 py-1.5 text-xs">{t.project_ids ? `${t.project_ids.length} only` : "all"}</td>
                <td className="px-3 py-1.5 text-xs">{t.status === "active" ? formatDate(t.expires_at) : <Badge tone={statusTone(t.status)}>{t.status}</Badge>}</td>
                <td className="px-3 py-1.5 text-xs text-muted">{t.last_used_at ? relativeTime(t.last_used_at) : "never"}</td>
                <td className="px-3 py-1.5 text-right">
                  {t.status === "active" && (
                    <Button
                      className="text-xs"
                      onClick={() => void revoke(t, `Disconnect “${t.oauth_client_name ?? t.name}”? It loses access to ${t.org_name ?? "the organisation"} at once.`)}
                      data-testid={`disconnect-${t.oauth_client_name ?? t.name}`}
                    >
                      Disconnect
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </Table>
        )}
      </div>
    </Panel>
  );
}

/** Organisation → every token scoped to it, for owners and admins (V2 §2.3). */
export function OrgTokensCard({ orgId }: { orgId: string }) {
  const q = useQuery({ queryKey: ["org", orgId, "tokens"], queryFn: () => api.orgTokens(orgId) });
  const qc = useQueryClient();
  const [err, setErr] = useState<string | null>(null);
  const revoke = async (t: APIToken) => {
    if (!window.confirm(`Revoke ${t.user_email}'s token “${t.name}”?`)) return;
    try {
      await api.revokeOrgToken(orgId, t.id);
      await qc.invalidateQueries({ queryKey: ["org", orgId, "tokens"] });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  return (
    <Panel title="API tokens">
      <div className="flex flex-col gap-3" data-testid="org-tokens">
        <p className="text-sm text-muted">Every member's tokens for this organisation. Revoking one stops it at once.</p>
        {q.data && (q.data.items.length ? <TokenRows items={q.data.items} revoke={revoke} showUser /> : <p className="text-sm text-muted">None yet.</p>)}
        {err && <Alert>{err}</Alert>}
      </div>
    </Panel>
  );
}

/** Platform settings → the longest token expiry (V2 §7.2). */
export function TokenSettingsCard() {
  const q = useQuery({ queryKey: ["admin", "token-settings"], queryFn: api.tokenSettings });
  const qc = useQueryClient();
  const [days, setDays] = useState<number | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  if (!q.data) return null;
  const save = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.putTokenSettings({ max_days: days ?? q.data.max_days });
      await qc.invalidateQueries({ queryKey: ["admin", "token-settings"] });
      setDays(null);
      setMsg({ ok: true, text: "Saved. Existing tokens keep their expiry." });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    }
  };
  return (
    <Panel title="API tokens">
      <form className="flex items-end gap-2" onSubmit={save}>
        <Field label="Longest expiry (days)" hint="Up to 365. New tokens can't be made to last longer.">
          {(id) => (
            <Input id={id} type="number" min={1} max={365} className="w-28" value={days ?? q.data.max_days} onChange={(e) => setDays(Number(e.target.value))} />
          )}
        </Field>
        <Button type="submit" disabled={days === null}>
          Save
        </Button>
      </form>
      {msg && <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>}
    </Panel>
  );
}
