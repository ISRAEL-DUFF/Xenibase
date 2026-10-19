import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { api, errorMessage, type InvitationCreated, type Org, type OrgMember, type OrgRole, type ProjectRole } from "../api/client";
import { Search } from "lucide-react";
import {
  Alert,
  Badge,
  Button,
  CopyField,
  Dialog,
  EmptyState,
  Field,
  FormRow,
  Input,
  Page,
  Panel,
  Section,
  Select,
  SidePanel,
  Switch,
  Table,
  PageSkeleton,
  TableSkeleton,
} from "../components/ui";
import { formatDate, relativeTime } from "../lib/format";
import { canManageOrg, setCurrentOrg, useCurrentOrg } from "../lib/org";
import { StorageTargetsPanel } from "../components/StorageTargets";
import { sessionQuery } from "../lib/session";
import { OrgTokensCard } from "../components/Tokens";
import { useTwoFactor } from "../lib/twofactor";

const orgRoles: OrgRole[] = ["owner", "admin", "member", "billing"];
const projectRoles: { id: ProjectRole; label: string }[] = [
  { id: "admin", label: "Admin" },
  { id: "developer", label: "Developer" },
  { id: "read_only", label: "Read-only" },
];

/** Org → Members (V2 §3.3). */
export function OrgMembersPage() {
  const { org } = useCurrentOrg();
  const { data: session } = useQuery(sessionQuery);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const members = useQuery({ queryKey: ["org", org?.id, "members"], queryFn: () => api.orgMembers(org!.id), enabled: !!org });
  const manage = canManageOrg(org);
  const invitations = useQuery({ queryKey: ["org", org?.id, "invitations"], queryFn: () => api.orgInvitations(org!.id), enabled: !!org && manage });
  const [inviting, setInviting] = useState(false);
  const [transferring, setTransferring] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  if (!org) return <PageSkeleton />;
  const owner = org.role === "owner";
  const refresh = () => qc.invalidateQueries({ queryKey: ["org", org.id] });

  const act = async (f: () => Promise<unknown>) => {
    setErr(null);
    try {
      await f();
      await refresh();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  const needle = search.trim().toLowerCase();
  const shown = (members.data?.items ?? []).filter((m) => !needle || m.email.toLowerCase().includes(needle) || (m.name ?? "").toLowerCase().includes(needle));

  return (
    <Page
      title="Members"
      description={`People in ${org.name}. Owners and admins manage everything; members see the projects they're added to.`}
      actions={
        <>
          {owner && (
            <Button onClick={() => setTransferring(true)} data-testid="transfer-ownership">
              Transfer ownership
            </Button>
          )}
          {!org.personal && (
            <Button
              onClick={() =>
                act(async () => {
                  await api.leaveOrg(org.id);
                  await qc.invalidateQueries({ queryKey: ["orgs"] });
                  const mine = (await api.orgs()).items.find((o) => o.personal);
                  if (mine) setCurrentOrg(mine.id);
                  await navigate({ to: "/projects" });
                })
              }
            >
              Leave
            </Button>
          )}
          {manage && (
            <Button variant="primary" onClick={() => setInviting(true)} data-testid="invite-member">
              Invite member
            </Button>
          )}
        </>
      }
    >
      <div className="relative">
        <Search className="pointer-events-none absolute top-1/2 left-2 h-3.5 w-3.5 -translate-y-1/2 text-muted" />
        <input
          type="search"
          placeholder="Filter members"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Filter members"
          className="h-[30px] w-64 rounded-md border border-line-strong bg-surface-2 pr-2 pl-7 text-[13px] text-fg placeholder:text-muted focus:border-accent focus:outline-none"
        />
      </div>
      {err && <Alert>{err}</Alert>}
      {members.isPending && <TableSkeleton cols={5} />}
      {members.data && (
        <Table head={["Member", "Role", "Projects", "2FA", "Last active", ""]}>
          {shown.map((m) => (
            <tr key={m.user_id} data-testid={`member-${m.email}`}>
              <td className="px-3 py-2">
                <div className="font-medium">{m.name || m.email}</div>
                {m.name && <div className="text-xs text-muted">{m.email}</div>}
                {m.user_id === session?.user?.id && <Badge tone="accent">you</Badge>}
              </td>
              <td className="px-3 py-2">
                {manage && m.user_id !== session?.user?.id ? (
                  <Select
                    aria-label={`Role of ${m.email}`}
                    value={m.role}
                    onChange={(e) => act(() => api.setOrgRole(org.id, m.user_id, e.target.value as OrgRole))}
                  >
                    {orgRoles
                      .filter((r) => owner || r !== "owner" || m.role === "owner")
                      .map((r) => (
                        <option key={r}>{r}</option>
                      ))}
                  </Select>
                ) : (
                  m.role
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                {m.role === "member"
                  ? m.projects.length
                    ? m.projects.map((p) => `${p.project_name} (${p.role.replace("_", "-")})`).join(", ")
                    : "none"
                  : "all (admin)"}
              </td>
              <td className="px-3 py-2">{m.totp_enabled ? <Badge tone="ok">on</Badge> : <Badge tone="warn">not set up</Badge>}</td>
              <td className="px-3 py-2 text-xs text-muted">{m.last_active_at ? relativeTime(m.last_active_at) : "—"}</td>
              <td className="px-3 py-2 text-right">
                {manage && m.user_id !== session?.user?.id && (owner || m.role !== "owner") && (
                  <Button
                    className="text-xs"
                    variant="danger"
                    onClick={() => act(() => api.removeOrgMember(org.id, m.user_id))}
                    data-testid={`remove-${m.email}`}
                  >
                    Remove
                  </Button>
                )}
              </td>
            </tr>
          ))}
        </Table>
      )}
      {manage && invitations.data && invitations.data.items.length > 0 && (
        <Section title="Pending invitations" description="Each link works once and expires after 7 days.">
          <Table head={["Email", "Role", "Invited by", "Expires", ""]}>
            {invitations.data.items.map((i) => (
              <tr key={i.id}>
                <td className="px-3 py-2">{i.email}</td>
                <td className="px-3 py-2">{i.role}</td>
                <td className="px-3 py-2 text-xs text-muted">{i.invited_by}</td>
                <td className="px-3 py-2 text-xs text-muted">{formatDate(i.expires_at)}</td>
                <td className="px-3 py-2 text-right">
                  <Button className="text-xs" onClick={() => act(() => api.revokeOrgInvitation(org.id, i.id))}>
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        </Section>
      )}
      <InviteModal
        orgId={org.id}
        canInviteOwners={owner}
        open={inviting}
        onClose={() => {
          setInviting(false);
          void refresh();
        }}
      />
      <TransferOwnershipModal
        orgId={org.id}
        members={(members.data?.items ?? []).filter((m) => m.user_id !== session?.user?.id)}
        open={transferring}
        onClose={() => {
          setTransferring(false);
          void refresh();
          void qc.invalidateQueries({ queryKey: ["orgs"] });
        }}
      />
    </Page>
  );
}

/** The result of an invitation: the link to pass on if email fails. */
export function InvitationResult({ created }: { created: InvitationCreated }) {
  return (
    <div className="flex flex-col gap-3" data-testid="invitation-created">
      {created.email_sent ? (
        <Alert tone="ok">Invitation emailed to {created.invitation.email}.</Alert>
      ) : (
        <Alert tone="warn">The email could not be sent ({created.email_error}). Send them this link yourself.</Alert>
      )}
      <CopyField label="Invitation link (valid 7 days, once)" value={created.url} testId="invitation-url" />
    </div>
  );
}

function InviteModal({ orgId, canInviteOwners, open, onClose }: { orgId: string; canInviteOwners: boolean; open: boolean; onClose: () => void }) {
  const projects = useQuery({ queryKey: ["projects", orgId], queryFn: () => api.projects(orgId), enabled: open });
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<OrgRole>("member");
  const [access, setAccess] = useState<Record<string, ProjectRole | "">>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [created, setCreated] = useState<InvitationCreated | null>(null);
  const close = () => {
    setEmail("");
    setRole("member");
    setAccess({});
    setCreated(null);
    setErr(null);
    onClose();
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const picks = Object.entries(access).filter(([, r]) => r) as [string, ProjectRole][];
      setCreated(
        await api.inviteOrgMember(orgId, { email, role, projects: role === "member" ? picks.map(([project_id, r]) => ({ project_id, role: r })) : [] }),
      );
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SidePanel
      title="Invite a member"
      description="They get an email with a link to join; you also get the link to pass on yourself."
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
            <Button type="submit" form="invite-form" variant="primary" busy={busy}>
              Send invitation
            </Button>
          </>
        )
      }
    >
      {created ? (
        <InvitationResult created={created} />
      ) : (
        <form id="invite-form" className="flex flex-col gap-4" onSubmit={submit}>
          <Field label="Email">{(id) => <Input id={id} type="email" required value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />}</Field>
          <Field label="Organisation role" hint="Owners and admins are admins of every project; members only of the projects you pick; billing members see billing and no projects.">
            {(id) => (
              <Select id={id} value={role} onChange={(e) => setRole(e.target.value as OrgRole)}>
                {orgRoles
                  .filter((r) => canInviteOwners || r !== "owner")
                  .map((r) => (
                    <option key={r}>{r}</option>
                  ))}
              </Select>
            )}
          </Field>
          {role === "member" && (projects.data?.items.length ?? 0) > 0 && (
            <fieldset className="flex flex-col overflow-hidden rounded-md border border-line">
              <legend className="sr-only">Project access</legend>
              <div className="border-b border-line bg-surface-2 px-3 py-2 text-[12px] text-muted">Project access</div>
              {projects.data!.items.map((p) => (
                <label key={p.id} className="flex items-center justify-between gap-2 border-b border-line px-3 py-2 text-[13px] last:border-0">
                  <span className="truncate">{p.name}</span>
                  <Select
                    aria-label={`Access to ${p.name}`}
                    value={access[p.id] ?? ""}
                    onChange={(e) => setAccess((a) => ({ ...a, [p.id]: e.target.value as ProjectRole | "" }))}
                  >
                    <option value="">No access</option>
                    {projectRoles.map((r) => (
                      <option key={r.id} value={r.id}>
                        {r.label}
                      </option>
                    ))}
                  </Select>
                </label>
              ))}
            </fieldset>
          )}
          {err && <Alert>{err}</Alert>}
        </form>
      )}
    </SidePanel>
  );
}

function TransferOwnershipModal({ orgId, members, open, onClose }: { orgId: string; members: OrgMember[]; open: boolean; onClose: () => void }) {
  const [to, setTo] = useState("");
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
      await api.transferOwnership(orgId, to);
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Transfer ownership" open={open} onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <p className="text-sm text-muted">They become an owner and you become an admin.</p>
        <Field label="New owner">
          {(id) => (
            <Select id={id} required value={to} onChange={(e) => setTo(e.target.value)}>
              <option value="">Choose a member…</option>
              {members.map((m) => (
                <option key={m.user_id} value={m.user_id}>
                  {m.name || m.email}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Your password">{(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
        {twoFactor && (
<Field label="Authenticator code">{(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
        )}
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" busy={busy} disabled={!to || !password || (twoFactor && code.length < 6)}>
            Transfer
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** Org → Settings (V2 §13). */
export function OrgSettingsPage() {
  const { org } = useCurrentOrg();
  const qc = useQueryClient();
  const [name, setName] = useState<string | null>(null);
  const [slug, setSlug] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!org) return <PageSkeleton />;
  if (!canManageOrg(org)) return <EmptyState title="Only owners and admins change organisation settings" />;
  const save = async (patch: Parameters<typeof api.updateOrg>[1]) => {
    setBusy(true);
    setErr(null);
    try {
      await api.updateOrg(org.id, patch);
      setName(null);
      setSlug(null);
      await qc.invalidateQueries({ queryKey: ["orgs"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const dirty = name !== null || slug !== null;
  return (
    <Page title="Organisation settings" description={org.personal ? "Your personal organisation. Invite others into it like any other." : undefined}>
      <div className="flex max-w-4xl flex-col gap-8">
        <Section title="General">
          <Panel
            footer={
              <>
                <Button
                  disabled={!dirty}
                  onClick={() => {
                    setName(null);
                    setSlug(null);
                  }}
                >
                  Cancel
                </Button>
                <Button type="submit" form="org-general" variant="primary" busy={busy} disabled={!dirty}>
                  Save
                </Button>
              </>
            }
          >
            <form
              id="org-general"
              onSubmit={(e) => {
                e.preventDefault();
                void save({ name: name ?? undefined, slug: slug ?? undefined });
              }}
            >
              <FormRow label="Organisation name" htmlFor="org-name">
                <Input id="org-name" aria-label="Name" value={name ?? org.name} onChange={(e) => setName(e.target.value)} maxLength={64} />
              </FormRow>
              <FormRow label="Slug" description="Used in the CLI and API." htmlFor="org-slug">
                <Input id="org-slug" aria-label="Slug" value={slug ?? org.slug} onChange={(e) => setSlug(e.target.value)} className="font-mono" />
              </FormRow>
              <FormRow label="Plan" description="Ask the platform admin to change it.">
                <div>
                  <Badge tone="accent">{org.plan}</Badge>
                </div>
              </FormRow>
            </form>
          </Panel>
        </Section>
        <Section title="Projects">
          <Panel>
            <FormRow label="Members can create projects" description="They become the admin of what they create.">
              <Switch
                checked={org.members_can_create_projects}
                onCheckedChange={(v) => void save({ members_can_create_projects: v })}
                aria-label="Members can create projects"
                data-testid="members-can-create"
              />
            </FormRow>
            <FormRow label="New projects contain sensitive data" description="Their branches copy the schema only unless a project admin asks for the data.">
              <Switch
                checked={!!org.sensitive_by_default}
                onCheckedChange={(v) => void save({ sensitive_by_default: v })}
                aria-label="New projects contain sensitive data"
                data-testid="sensitive-by-default"
              />
            </FormRow>
          </Panel>
        </Section>
        {err && <Alert>{err}</Alert>}
        <OrgTokensCard orgId={org.id} />
        <Section title="Backup storage" description="Where this organisation's projects keep their backups, unless a project picks its own.">
          <Panel>
            <StorageTargetsPanel org={org.id} />
          </Panel>
        </Section>
        {org.role === "owner" && !org.personal && org.status === "active" && (
          <Section title="Danger zone">
            <DeleteOrgCard org={org} />
          </Section>
        )}
      </div>
    </Page>
  );
}

/** Delete the organisation, after a 7-day grace period (V2 §10.10). */
function DeleteOrgCard({ org }: { org: Org }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [all, setAll] = useState(false);
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
      await api.deleteOrg(org.id, { confirm, delete_projects: all });
      setOpen(false);
      await qc.invalidateQueries();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel tone="danger">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-[13px]">Delete this organisation</p>
          <p className="mt-0.5 text-[12px] text-muted">
            Its projects go offline at once, and are deleted (each with a final backup kept for 30 days) after 7 days. Until then any owner can cancel.
          </p>
        </div>
        <Button variant="danger" onClick={() => setOpen(true)} data-testid="delete-org">
          Delete…
        </Button>
      </div>
      <Dialog title={`Delete ${org.name}`} open={open} onOpenChange={setOpen}>
        <form className="flex flex-col gap-3" onSubmit={submit}>
          <Field label={`Type ${org.name} to confirm`}>{(id) => <Input id={id} value={confirm} onChange={(e) => setConfirm(e.target.value)} />}</Field>
          {org.project_count > 0 && (
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> Delete its {org.project_count} project
              {org.project_count === 1 ? "" : "s"} too
            </label>
          )}
          <Field label="Your password">{(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
          {twoFactor && (
<Field label="Authenticator code">{(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
          )}
          {err && <Alert>{err}</Alert>}
          <Button type="submit" variant="danger" busy={busy} disabled={confirm !== org.name || (org.project_count > 0 && !all) || !password || (twoFactor && code.length < 6)}>
            Delete in 7 days
          </Button>
        </form>
      </Dialog>
    </Panel>
  );
}
