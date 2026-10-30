import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { LifeBuoy, MessageCircle, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import {
  api,
  errorMessage,
  type SupportContext,
  type Ticket,
  type TicketDetail,
  type TicketMessage,
} from "../api/client";
import {
  Alert,
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  KeyValues,
  Page,
  PageSkeleton,
  Panel,
  Segmented,
  Select,
  SidePanel,
  Stat,
  Table,
  TableSkeleton,
  type Tone,
} from "../components/ui";
import { naira } from "../lib/billing";
import { formatDate, relativeTime } from "../lib/format";
import { canManageOrg, useCurrentOrg } from "../lib/org";
import { sessionQuery } from "../lib/session";

const STATUS_TONE: Record<Ticket["status"], Tone> = {
  open: "accent",
  pending: "warn",
  solved: "ok",
  closed: "muted",
};
const PRIORITY_TONE: Record<Ticket["priority"], Tone> = {
  low: "muted",
  normal: "muted",
  high: "warn",
  urgent: "danger",
};
const CHANNEL_LABEL: Record<Ticket["channel"], string> = {
  dashboard: "dashboard",
  email: "email",
  whatsapp: "WhatsApp",
};

const textarea =
  "min-h-28 w-full rounded-md border border-line-strong bg-surface-2 p-2 text-[13px] text-fg focus:border-accent focus:outline-none";

/** When a ticket is first due an answer: "due in 3h", "overdue", or nothing
 * once answered or for best effort. */
function dueLabel(
  t: Ticket,
  now = Date.now(),
): { text: string; tone: Tone } | null {
  if (
    t.first_response_at ||
    !t.respond_by ||
    t.status === "solved" ||
    t.status === "closed"
  )
    return null;
  const due = new Date(t.respond_by).getTime();
  if (due < now) return { text: "overdue", tone: "danger" };
  return {
    text: `due ${relativeTime(t.respond_by, now)}`,
    tone: due - now < 3600_000 ? "warn" : "muted",
  };
}

/** A ticket's conversation. Internal notes show only in the console. */
function Thread({ messages }: { messages: TicketMessage[] }) {
  return (
    <ol className="flex flex-col gap-3" data-testid="ticket-thread">
      {messages.map((m) =>
        m.direction === "note" && m.author === "system" ? (
          // Xenibase's own record of an email it sent (kept to thread replies).
          <li
            key={m.id}
            className="text-center text-[11px] text-muted"
            data-testid="message-system"
          >
            {m.body} · {formatDate(m.created_at)}
          </li>
        ) : (
          <li
            key={m.id}
            className={
              m.direction === "note"
                ? "rounded-md border border-warn/40 bg-warn/5 p-3"
                : m.direction === "out"
                  ? "ml-6 rounded-md border border-accent/30 bg-accent/5 p-3"
                  : "mr-6 rounded-md border border-line bg-surface-2 p-3"
            }
            data-testid={`message-${m.direction}`}
          >
            <div className="mb-1 flex flex-wrap items-center gap-2 text-xs text-muted">
              <span className="font-medium text-fg">{m.author}</span>
              {m.direction === "note" && (
                <Badge tone="warn">internal note</Badge>
              )}
              <span>{formatDate(m.created_at)}</span>
            </div>
            <div className="text-[13px] whitespace-pre-wrap">{m.body}</div>
          </li>
        ),
      )}
    </ol>
  );
}

function TicketBadges({ t }: { t: Ticket }) {
  const due = dueLabel(t);
  return (
    <span className="flex flex-wrap items-center gap-1">
      <Badge tone={STATUS_TONE[t.status]}>{t.status}</Badge>
      {t.priority !== "normal" && (
        <Badge tone={PRIORITY_TONE[t.priority]}>{t.priority}</Badge>
      )}
      {due && <Badge tone={due.tone}>{due.text}</Badge>}
      {t.unverified_sender && (
        <Badge tone="danger">unverified sender</Badge>
      )}
    </span>
  );
}

/** Org → Support (V3 §7.1): the organisation's tickets, opening one, and
 * the WhatsApp numbers that may message support. */
export function SupportPage() {
  const { org } = useCurrentOrg();
  const qc = useQueryClient();
  const tickets = useQuery({
    queryKey: ["org", org?.id, "tickets"],
    queryFn: () => api.orgTickets(org!.id),
    enabled: !!org,
  });
  const [opening, setOpening] = useState(false);
  const [openId, setOpenId] = useState<string | null>(null);
  if (!org) return <PageSkeleton />;
  return (
    <Page
      title="Support"
      description={
        <>
          Ask us anything about your projects or your bill. You can also email
          us, and Pro and Team organisations can message us on WhatsApp from a
          registered number. First responses are in Nigerian business hours:
          Free best effort, Pro within a business day, Team within four business
          hours; urgent issues on a paid plan within an hour, at any time.
        </>
      }
      testId="support-page"
      actions={
        <Button
          variant="primary"
          icon={<LifeBuoy className="h-3.5 w-3.5" />}
          onClick={() => setOpening(true)}
          data-testid="new-ticket"
        >
          New ticket
        </Button>
      }
    >
      {tickets.isPending && <TableSkeleton cols={4} />}
      {tickets.error && <Alert>{errorMessage(tickets.error)}</Alert>}
      {tickets.data && tickets.data.items.length === 0 && (
        <EmptyState title="No tickets yet" icon={<LifeBuoy />}>
          {canManageOrg(org)
            ? "Your organisation's tickets show here."
            : "Tickets you open show here."}
        </EmptyState>
      )}
      {tickets.data && tickets.data.items.length > 0 && (
        <Table head={["Ticket", "Status", "Channel", "Updated"]}>
          {tickets.data.items.map((t) => (
            <tr
              key={t.id}
              className="cursor-pointer"
              onClick={() => setOpenId(t.id)}
              data-testid={`ticket-${t.ref}`}
            >
              <td className="px-3 py-2">
                <div className="font-medium">{t.subject}</div>
                <div className="text-xs text-muted">
                  {t.ref} · {t.requester}
                </div>
              </td>
              <td className="px-3 py-2">
                <TicketBadges t={t} />
              </td>
              <td className="px-3 py-2 text-xs">{CHANNEL_LABEL[t.channel]}</td>
              <td className="px-3 py-2 text-xs text-muted">
                {relativeTime(t.updated_at)}
              </td>
            </tr>
          ))}
        </Table>
      )}
      <WhatsAppNumbers orgId={org.id} manager={canManageOrg(org)} />
      <NewTicket
        orgId={org.id}
        open={opening}
        onClose={() => setOpening(false)}
        onOpened={(t) => {
          setOpening(false);
          setOpenId(t.id);
          void qc.invalidateQueries({ queryKey: ["org", org.id, "tickets"] });
        }}
      />
      <OrgTicket orgId={org.id} id={openId} onClose={() => setOpenId(null)} />
    </Page>
  );
}

function NewTicket({
  orgId,
  open,
  onClose,
  onOpened,
}: {
  orgId: string;
  open: boolean;
  onClose: () => void;
  onOpened: (t: Ticket) => void;
}) {
  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  const [urgent, setUrgent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const t = await api.openTicket(orgId, {
        subject,
        body,
        priority: urgent ? "urgent" : "normal",
      });
      setSubject("");
      setBody("");
      setUrgent(false);
      onOpened(t);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SidePanel
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title="New support ticket"
      description="We email you when we answer; reply to that email or here."
      testId="new-ticket-panel"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="new-ticket-form"
            busy={busy}
            disabled={!subject.trim() || !body.trim()}
            data-testid="ticket-submit"
          >
            Send
          </Button>
        </>
      }
    >
      <form
        id="new-ticket-form"
        className="flex flex-col gap-4"
        onSubmit={submit}
      >
        <Field label="Subject">
          {(id) => (
            <Input
              id={id}
              value={subject}
              maxLength={200}
              onChange={(e) => setSubject(e.target.value)}
              data-testid="ticket-subject"
            />
          )}
        </Field>
        <Field
          label="What's happening?"
          hint="Which project, what you expected, what you saw, and since when."
        >
          {(id) => (
            <textarea
              id={id}
              className={textarea}
              rows={8}
              value={body}
              onChange={(e) => setBody(e.target.value)}
              data-testid="ticket-body"
            />
          )}
        </Field>
        <label className="flex items-start gap-2 text-sm">
          <input
            type="checkbox"
            className="mt-1"
            checked={urgent}
            onChange={(e) => setUrgent(e.target.checked)}
          />
          <span>
            Urgent: production is down or data is at risk.
            <span className="block text-xs text-muted">
              On a paid plan we answer urgent tickets within an hour, at any
              time.
            </span>
          </span>
        </label>
        {err && <Alert>{err}</Alert>}
      </form>
    </SidePanel>
  );
}

function OrgTicket({
  orgId,
  id,
  onClose,
}: {
  orgId: string;
  id: string | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["org", orgId, "ticket", id],
    queryFn: () => api.orgTicket(orgId, id!),
    enabled: !!id,
  });
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const send = async () => {
    if (!id) return;
    setBusy(true);
    setErr(null);
    try {
      await api.replyTicket(orgId, id, body);
      setBody("");
      await qc.invalidateQueries({ queryKey: ["org", orgId] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const t = q.data?.ticket;
  return (
    <SidePanel
      open={!!id}
      onOpenChange={(o) => !o && onClose()}
      title={t ? `${t.ref}: ${t.subject}` : "Ticket"}
      description={t && <TicketBadges t={t} />}
      size="large"
      testId="ticket-panel"
      footer={
        <Button
          variant="primary"
          busy={busy}
          disabled={!body.trim()}
          onClick={() => void send()}
          data-testid="ticket-reply-send"
        >
          Reply
        </Button>
      }
    >
      {q.data ? (
        <div className="flex flex-col gap-4">
          <Thread messages={q.data.messages} />
          <Field label="Your reply">
            {(fid) => (
              <textarea
                id={fid}
                className={textarea}
                value={body}
                onChange={(e) => setBody(e.target.value)}
                data-testid="ticket-reply"
              />
            )}
          </Field>
          {err && <Alert>{err}</Alert>}
        </div>
      ) : (
        <TableSkeleton rows={3} cols={1} />
      )}
    </SidePanel>
  );
}

/** The numbers that may message support on WhatsApp (Pro and Team). */
function WhatsAppNumbers({
  orgId,
  manager,
}: {
  orgId: string;
  manager: boolean;
}) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["org", orgId, "support-phones"],
    queryFn: () => api.supportPhones(orgId),
  });
  const [phone, setPhone] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const act = async (f: () => Promise<unknown>) => {
    setErr(null);
    try {
      await f();
      setPhone("");
      await qc.invalidateQueries({
        queryKey: ["org", orgId, "support-phones"],
      });
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  if (!q.data) return null;
  return (
    <Panel
      title="WhatsApp"
      description={
        q.data.available
          ? "Messages to our support number from these numbers open a ticket for this organisation, and we answer there. A new number works once its code is sent from it to our support number on WhatsApp."
          : "WhatsApp support comes with the Pro and Team plans."
      }
      testId="support-phones"
    >
      {q.data.available ? (
        <div className="flex flex-col gap-3">
          {q.data.items.length === 0 && (
            <p className="text-sm text-muted">No numbers registered.</p>
          )}
          {q.data.items.map((p) => (
            <div
              key={p.phone}
              className="flex items-center justify-between text-sm"
            >
              <span className="flex flex-wrap items-center gap-2">
                <MessageCircle className="h-4 w-4 text-muted" />
                {p.phone}
                {p.verified ? (
                  <Badge tone="ok">confirmed</Badge>
                ) : (
                  <>
                    <Badge tone="warn">waiting for code</Badge>
                    {p.code && (
                      <span
                        className="text-xs text-muted"
                        data-testid="support-phone-code"
                      >
                        Send <code className="font-mono">{p.code}</code> from
                        this number to Xenibase support on WhatsApp
                        {p.code_expires_at &&
                          ` by ${formatDate(p.code_expires_at)}`}
                        .
                      </span>
                    )}
                  </>
                )}
              </span>
              {manager && (
                <Button
                  size="tiny"
                  variant="ghost"
                  icon={<Trash2 className="h-3.5 w-3.5" />}
                  onClick={() =>
                    void act(() => api.removeSupportPhone(orgId, p.phone))
                  }
                  aria-label={`Remove ${p.phone}`}
                />
              )}
            </div>
          ))}
          {manager && (
            <form
              className="flex flex-wrap gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                void act(() => api.addSupportPhone(orgId, phone));
              }}
            >
              <Input
                placeholder="+234 803 123 4567"
                value={phone}
                onChange={(e) => setPhone(e.target.value)}
                className="max-w-xs"
                aria-label="WhatsApp number"
                data-testid="support-phone"
              />
              <Button type="submit" disabled={!phone.trim()}>
                Add number
              </Button>
            </form>
          )}
          {err && <Alert>{err}</Alert>}
        </div>
      ) : (
        <Link
          to="/pricing"
          className="text-sm text-accent-text hover:underline"
        >
          See the plans
        </Link>
      )}
    </Panel>
  );
}

type Filter = "open" | "pending" | "mine" | "all";

/** Platform → Support (V3 §7.1): every ticket from the dashboard, email and
 * WhatsApp in one queue, with the organisation's context beside the
 * thread. Support staff and platform admins. */
export function SupportConsolePage() {
  const [filter, setFilter] = useState<Filter>("open");
  const [openId, setOpenId] = useState<string | null>(null);
  const me = useQuery(sessionQuery);
  const params =
    filter === "all"
      ? {}
      : filter === "mine"
        ? { assignee: me.data?.user?.id }
        : { status: filter };
  const q = useQuery({
    queryKey: ["support", "queue", filter],
    queryFn: () => api.supportQueue(params),
    enabled: filter !== "mine" || !!me.data,
  });
  const staff = useQuery({
    queryKey: ["support", "staff"],
    queryFn: api.supportStaff,
  });
  const staffName = (id?: string | null) =>
    staff.data?.items.find((s) => s.id === id)?.email;
  return (
    <Page
      title="Support"
      description="Open tickets come first, by when they're due an answer. Replies go back by the channel the ticket came in on; internal notes stay here."
      testId="support-console"
      actions={
        <Segmented<Filter>
          value={filter}
          onChange={setFilter}
          options={[
            { value: "open", label: "Open" },
            { value: "pending", label: "Pending" },
            { value: "mine", label: "Mine" },
            { value: "all", label: "All" },
          ]}
        />
      }
    >
      {q.data && (
        <div className="grid gap-3 sm:grid-cols-3">
          <Stat label="Open" value={q.data.open} testId="support-open" />
          <Stat label="Waiting on the customer" value={q.data.pending} />
          <Stat label="Overdue" value={q.data.overdue} />
        </div>
      )}
      {q.isPending && <TableSkeleton cols={5} />}
      {q.error && <Alert>{errorMessage(q.error)}</Alert>}
      {q.data && q.data.items.length === 0 && (
        <EmptyState title="Nothing here" icon={<LifeBuoy />} />
      )}
      {q.data && q.data.items.length > 0 && (
        <Table
          head={["Ticket", "Organisation", "Status", "Assignee", "Updated"]}
        >
          {q.data.items.map((t) => (
            <tr
              key={t.id}
              className="cursor-pointer"
              onClick={() => setOpenId(t.id)}
              data-testid={`console-${t.ref}`}
            >
              <td className="px-3 py-2">
                <div className="font-medium">{t.subject}</div>
                <div className="text-xs text-muted">
                  {t.ref} · {CHANNEL_LABEL[t.channel]} ·{" "}
                  {t.requester_name ? `${t.requester_name} ` : ""}
                  {t.requester}
                </div>
              </td>
              <td className="px-3 py-2 text-xs">
                {t.org_name ?? <span className="text-muted">unknown</span>}
                {t.plan && <div className="text-muted">{t.plan}</div>}
              </td>
              <td className="px-3 py-2">
                <TicketBadges t={t} />
              </td>
              <td className="px-3 py-2 text-xs">
                {staffName(t.assignee) ?? <span className="text-muted">—</span>}
              </td>
              <td className="px-3 py-2 text-xs text-muted">
                {relativeTime(t.updated_at)}
              </td>
            </tr>
          ))}
        </Table>
      )}
      <ConsoleTicket id={openId} onClose={() => setOpenId(null)} />
    </Page>
  );
}

function ConsoleTicket({
  id,
  onClose,
}: {
  id: string | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["support", "ticket", id],
    queryFn: () => api.supportTicket(id!),
    enabled: !!id,
  });
  const staff = useQuery({
    queryKey: ["support", "staff"],
    queryFn: api.supportStaff,
  });
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const act = async (key: string, f: () => Promise<unknown>) => {
    setBusy(key);
    setErr(null);
    try {
      await f();
      await qc.invalidateQueries({ queryKey: ["support"] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const d: TicketDetail | undefined = q.data;
  const t = d?.ticket;
  return (
    <SidePanel
      open={!!id}
      onOpenChange={(o) => !o && onClose()}
      title={t ? `${t.ref}: ${t.subject}` : "Ticket"}
      description={
        t &&
        `${CHANNEL_LABEL[t.channel]} · ${t.requester}${t.unverified_sender ? " (unverified sender)" : ""} · opened ${formatDate(t.created_at)}`
      }
      size="xlarge"
      testId="console-ticket"
      footer={
        <>
          <Button
            busy={busy === "note"}
            disabled={!body.trim()}
            onClick={() =>
              void act(
                "note",
                async () => (
                  await api.staffReply(id!, body, true),
                  setBody("")
                ),
              )
            }
            data-testid="console-note"
          >
            Add note
          </Button>
          <Button
            variant="primary"
            busy={busy === "reply"}
            disabled={!body.trim()}
            onClick={() =>
              void act(
                "reply",
                async () => (
                  await api.staffReply(id!, body, false),
                  setBody("")
                ),
              )
            }
            data-testid="console-reply"
          >
            Reply to customer
          </Button>
        </>
      }
    >
      {d && t ? (
        <div className="grid gap-5 lg:grid-cols-[1fr_18rem]">
          <div className="flex min-w-0 flex-col gap-4">
            {t.unverified_sender && (
              <Alert tone="warn">
                The email provider couldn&apos;t authenticate this sender (no
                DMARC pass), so the From address may be forged. It isn&apos;t
                linked to an organisation and replies to it never join it.
                Don&apos;t act on what it asks without checking with the
                organisation another way.
              </Alert>
            )}
            <div className="flex flex-wrap items-center gap-2">
              <Select
                aria-label="Status"
                value={t.status}
                onChange={(e) =>
                  void act("status", () =>
                    api.updateTicket(t.id, {
                      status: e.target.value as Ticket["status"],
                    }),
                  )
                }
                data-testid="console-status"
              >
                {(["open", "pending", "solved", "closed"] as const).map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </Select>
              <Select
                aria-label="Priority"
                value={t.priority}
                onChange={(e) =>
                  void act("priority", () =>
                    api.updateTicket(t.id, {
                      priority: e.target.value as Ticket["priority"],
                    }),
                  )
                }
              >
                {(["low", "normal", "high", "urgent"] as const).map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </Select>
              <Select
                aria-label="Assignee"
                value={t.assignee ?? ""}
                onChange={(e) =>
                  void act("assign", () =>
                    api.updateTicket(
                      t.id,
                      e.target.value
                        ? { assignee: e.target.value }
                        : { clear_assignee: true },
                    ),
                  )
                }
                data-testid="console-assignee"
              >
                <option value="">Unassigned</option>
                {staff.data?.items.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name || s.email}
                  </option>
                ))}
              </Select>
              <TicketBadges t={t} />
            </div>
            <Thread messages={d.messages} />
            <Field
              label="Reply or note"
              hint="A reply goes to the customer by the ticket's channel; a note is for staff only."
            >
              {(fid) => (
                <textarea
                  id={fid}
                  className={textarea}
                  rows={6}
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  data-testid="console-body"
                />
              )}
            </Field>
            {err && <Alert>{err}</Alert>}
          </div>
          <OrgContext c={d.context} />
        </div>
      ) : (
        <TableSkeleton rows={4} cols={1} />
      )}
    </SidePanel>
  );
}

/** What support sees of the organisation beside a ticket: plan, billing
 * standing, members, projects, recent operations and incidents. */
function OrgContext({ c }: { c?: SupportContext }) {
  if (!c) {
    return (
      <Panel title="Organisation">
        <p className="text-sm text-muted">
          This sender isn't linked to an organisation.
        </p>
      </Panel>
    );
  }
  return (
    <div
      className="flex flex-col gap-3 text-[13px]"
      data-testid="console-context"
    >
      <Panel
        title={c.org_name}
        description={`${c.plan}${c.term ? `, ${c.term}` : ""} · ${c.org_status}`}
      >
        <KeyValues
          items={[
            ["Members", c.members],
            ["Billing", c.billing_mode ?? "—"],
            ["Dunning", c.dunning_state ?? "none"],
            ["Owed", c.owed_minor != null ? naira(c.owed_minor) : "—"],
            ["Credit", c.credit_minor != null ? naira(c.credit_minor) : "—"],
          ]}
        />
      </Panel>
      <Panel title={`Projects (${c.projects.length})`}>
        {c.projects.length === 0 && <p className="text-muted">None.</p>}
        <ul className="flex flex-col gap-1">
          {c.projects.map((p) => (
            <li key={p.id} className="flex justify-between gap-2">
              <span className="truncate">{p.name}</span>
              <span className="text-xs text-muted">
                {p.tier} · {p.lifecycle === "active" ? p.status : p.lifecycle}
              </span>
            </li>
          ))}
        </ul>
      </Panel>
      {c.recent_operations.length > 0 && (
        <Panel title="Recent operations">
          <ul className="flex flex-col gap-1">
            {c.recent_operations.map((o) => (
              <li key={o.id} className="flex justify-between gap-2">
                <span className="truncate">{o.kind}</span>
                <Badge
                  tone={
                    o.status === "failed"
                      ? "danger"
                      : o.status === "succeeded"
                        ? "ok"
                        : "muted"
                  }
                >
                  {o.status}
                </Badge>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      {c.incidents.length > 0 && (
        <Panel title="Open incidents" tone="warn">
          <ul className="flex flex-col gap-1">
            {c.incidents.map((i) => (
              <li key={i.id}>
                {i.title}{" "}
                <span className="text-xs text-muted">({i.severity})</span>
              </li>
            ))}
          </ul>
        </Panel>
      )}
    </div>
  );
}
