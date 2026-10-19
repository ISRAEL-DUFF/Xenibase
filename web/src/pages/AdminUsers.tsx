import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api, errorMessage, type AdminUser, type InvitationCreated } from "../api/client";
import { Alert, Badge, Button, Field, Input, Dialog, Select, PageHeading, SidePanel, Table, TableSkeleton } from "../components/ui";
import { formatDate, relativeTime } from "../lib/format";
import { sessionQuery } from "../lib/session";
import { InvitationResult } from "./Org";

/** Admin → Users (V2 §2.4, §3.1, §3.6). */
export function AdminUsersPage() {
  const { data: session } = useQuery(sessionQuery);
  const qc = useQueryClient();
  const [search, setSearch] = useState("");
  const [pending, setPending] = useState(false);
  const users = useQuery({ queryKey: ["admin", "users", { search, pending }], queryFn: () => api.users({ q: search || undefined, pending }) });
  const invitations = useQuery({ queryKey: ["admin", "invitations"], queryFn: api.platformInvitations });
  const [inviting, setInviting] = useState(false);
  const [resetting, setResetting] = useState<AdminUser | null>(null);
  const [roleFor, setRoleFor] = useState<AdminUser | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const act = async (f: () => Promise<unknown>) => {
    setErr(null);
    try {
      await f();
      await qc.invalidateQueries({ queryKey: ["admin"] });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  return (
    <>
      <PageHeading
        title="Users"
        description="Every account on this Xenibase. You see who they are, not what is in their organisations."
        actions={
          <Button variant="primary" onClick={() => setInviting(true)} data-testid="invite-user">
            Invite someone
          </Button>
        }
      />
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <Input placeholder="Search email or name" value={search} onChange={(e) => setSearch(e.target.value)} className="max-w-xs" aria-label="Search users" />
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={pending} onChange={(e) => setPending(e.target.checked)} /> Waiting for approval
        </label>
      </div>
      {err && (
        <div className="mb-3">
          <Alert>{err}</Alert>
        </div>
      )}
      {users.isPending && <TableSkeleton cols={5} />}
      {users.data && (
        <Table head={["User", "State", "2FA", "Orgs", "Last active", ""]}>
          {users.data.items.map((u) => (
            <tr key={u.id} data-testid={`user-${u.email}`}>
              <td className="px-3 py-2">
                <div className="font-medium">{u.name || u.email}</div>
                <div className="text-xs text-muted">
                  {u.name ? u.email + " · " : ""}
                  {ROLE_LABEL[u.platform_role] ?? "user"} · joined {formatDate(u.created_at)}
                </div>
              </td>
              <td className="px-3 py-2">
                {u.disabled ? (
                  <Badge tone="danger">disabled</Badge>
                ) : !u.email_verified ? (
                  <Badge tone="warn">unverified</Badge>
                ) : !u.approved ? (
                  <Badge tone="warn">awaiting approval</Badge>
                ) : (
                  <Badge tone="ok">active</Badge>
                )}
              </td>
              <td className="px-3 py-2">{u.totp_enabled ? "on" : "—"}</td>
              <td className="px-3 py-2">{u.org_count}</td>
              <td className="px-3 py-2 text-xs text-muted">{u.last_active_at ? relativeTime(u.last_active_at) : "—"}</td>
              <td className="px-3 py-2">
                <div className="flex justify-end gap-1">
                  {!u.approved && !u.disabled && (
                    <Button className="text-xs" variant="primary" onClick={() => act(() => api.updateUser(u.id, { approved: true }))}>
                      Approve
                    </Button>
                  )}
                  {u.totp_enabled && u.id !== session?.user?.id && (
                    <Button className="text-xs" onClick={() => setResetting(u)}>
                      Reset 2FA
                    </Button>
                  )}
                  {u.id !== session?.user?.id && u.approved && u.email_verified && !u.disabled && (u.totp_enabled || u.platform_role !== "user") && (
                    <Button className="text-xs" onClick={() => setRoleFor(u)} data-testid={`role-${u.email}`}>
                      Platform role
                    </Button>
                  )}
                  {u.id !== session?.user?.id && (
                    <Button
                      className="text-xs"
                      variant={u.disabled ? "primary" : "danger"}
                      onClick={() => act(() => api.updateUser(u.id, { disabled: !u.disabled }))}
                    >
                      {u.disabled ? "Enable" : "Disable"}
                    </Button>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </Table>
      )}
      {invitations.data && invitations.data.items.length > 0 && (
        <div className="mt-6">
          <h2 className="mb-2 font-semibold">Pending platform invitations</h2>
          <Table head={["Email", "Invited", "Expires", ""]}>
            {invitations.data.items.map((i) => (
              <tr key={i.id}>
                <td className="px-3 py-2">{i.email}</td>
                <td className="px-3 py-2 text-xs text-muted">{formatDate(i.created_at)}</td>
                <td className="px-3 py-2 text-xs text-muted">{formatDate(i.expires_at)}</td>
                <td className="px-3 py-2 text-right">
                  <Button className="text-xs" onClick={() => act(() => api.revokePlatformInvitation(i.id))}>
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </Table>
        </div>
      )}
      <PlatformInviteModal
        open={inviting}
        onClose={() => {
          setInviting(false);
          void qc.invalidateQueries({ queryKey: ["admin"] });
        }}
      />
      <ResetTotpModal user={resetting} onClose={() => setResetting(null)} />
      <PlatformRoleModal user={roleFor} onClose={() => setRoleFor(null)} />
    </>
  );
}

function PlatformInviteModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [created, setCreated] = useState<InvitationCreated | null>(null);
  const close = () => {
    setEmail("");
    setCreated(null);
    setErr(null);
    onClose();
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      setCreated(await api.invitePlatform(email));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SidePanel
      title="Invite someone to Xenibase"
      description="They get an account and a personal organisation, and no access to anyone else's."
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
            <Button type="submit" form="platform-invite" variant="primary" busy={busy}>
              Send invitation
            </Button>
          </>
        )
      }
    >
      {created ? (
        <InvitationResult created={created} />
      ) : (
        <form id="platform-invite" className="flex flex-col gap-4" onSubmit={submit}>
          <Field label="Email">{(id) => <Input id={id} type="email" required value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />}</Field>
          {err && <Alert>{err}</Alert>}
        </form>
      )}
    </SidePanel>
  );
}

function ResetTotpModal({ user, onClose }: { user: AdminUser | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [confirmed, setConfirmed] = useState(false);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!user) return;
    setBusy(true);
    setErr(null);
    try {
      await api.reauth({ password, code });
      await api.resetUserTotp(user.id);
      await qc.invalidateQueries({ queryKey: ["admin"] });
      setConfirmed(false);
      setPassword("");
      setCode("");
      onClose();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title="Reset two-factor authentication" open={!!user} onOpenChange={(o) => !o && onClose()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <p className="text-sm text-muted">
          {user?.email} is emailed now. Platform staff set up a new authenticator at their next sign-in; anyone else signs in with a password until they turn two-factor on again. Only do this after confirming who they are some other way.
        </p>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} /> I've confirmed their identity out of band
        </label>
        <Field label="Your password">{(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
        <Field label="Your authenticator code">{(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="danger" busy={busy} disabled={!confirmed || !password || code.length < 6}>
            Reset 2FA
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

const ROLE_LABEL: Record<string, string> = { user: "user", platform_admin: "platform admin", support: "support staff" };
type PlatformRole = "user" | "support" | "platform_admin";

/** Changes an account's platform role: a platform admin, support staff
 * (the support console and organisation metadata, V3 §7.1), or an
 * ordinary user. It asks for the signed-in admin's password and a code
 * first. */
function PlatformRoleModal({ user, onClose }: { user: AdminUser | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [role, setRole] = useState<PlatformRole | "">("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const current = (user?.platform_role ?? "user") as PlatformRole;
  const target = role || (current === "user" ? "platform_admin" : "user");
  const close = () => {
    setRole("");
    setPassword("");
    setCode("");
    setErr(null);
    onClose();
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!user) return;
    setBusy(true);
    setErr(null);
    try {
      await api.reauth({ password, code });
      await api.updateUser(user.id, { platform_role: target });
      await qc.invalidateQueries({ queryKey: ["admin"] });
      close();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const effect: Record<PlatformRole, string> = {
    platform_admin: "will be able to manage users, plans, nodes, billing and the platform's settings, and will see the platform admin area.",
    support: "will see the support console: tickets, and the metadata of the organisations that raise them (plan, members, projects, recent operations). Nothing else of the platform's.",
    user: "will lose access to the platform's areas; their own organisations and projects stay as they are. The last platform admin can't be removed.",
  };
  return (
    <Dialog title={`Platform role of ${user?.email ?? ""}`} open={!!user} onOpenChange={(o) => !o && close()}>
      <form className="flex flex-col gap-4" onSubmit={submit}>
        <Field label="Role">
          {(id) => (
            <Select id={id} value={target} onChange={(e) => setRole(e.target.value as PlatformRole)} data-testid="role-select">
              {(["user", "support", "platform_admin"] as const)
                .filter((r) => r !== current)
                .map((r) => (
                  <option key={r} value={r}>
                    {ROLE_LABEL[r]}
                  </option>
                ))}
            </Select>
          )}
        </Field>
        <p className="text-sm text-muted">
          {user?.email} {effect[target]} They are signed out and emailed.
        </p>
        <Field label="Your password">{(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} />}</Field>
        <Field label="Your authenticator code">{(id) => <Input id={id} inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} />}</Field>
        {err && <Alert>{err}</Alert>}
        <div className="flex justify-end gap-2">
          <Button onClick={close}>Cancel</Button>
          <Button type="submit" variant={target === "user" ? "danger" : "primary"} busy={busy} disabled={!password || code.length < 6} data-testid="role-confirm">
            Change role
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
