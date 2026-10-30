import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearch } from "@tanstack/react-router";
import { Download } from "lucide-react";
import { useEffect, useState } from "react";
import {
  api,
  errorMessage,
  type BillingAccount,
  type Invoice,
  type InvoiceLine,
  type PlanChange,
  type PlanOption,
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
  Select,
  SidePanel,
  Stat,
  Table,
} from "../components/ui";
import {
  INVOICE_STATUS,
  approxNaira,
  bps,
  nairaRate,
  naira,
  parseNaira,
  periodLabel,
  rate,
  usd,
  usdUnit,
} from "../lib/billing";
import { formatDate } from "../lib/format";
import { setCurrentOrg, useCurrentOrg } from "../lib/org";
import {
  InvoicePay,
  PaymentsPanel,
  StandingBanner,
  WalletPanel,
} from "../components/BillingPayments";
import { LegalBanner } from "./Legal";

/** The billing page's names for each service (V4 §12). */
export const SERVICE_LABELS: Record<string, string> = {
  plan: "Plan",
  database: "Databases",
  data_api: "Data API",
  auth: "Auth",
  messages: "SMS and WhatsApp",
  storage: "File storage",
  realtime: "Realtime",
  read_replicas: "Read replicas",
};

/** Owners and billing members see billing (V3 §3.2). */
export function canSeeBilling(role: string | undefined): boolean {
  return role === "owner" || role === "billing";
}

/** An invoice's lines. Lines priced in dollars show the dollar amount with
 * the naira it converted to; older naira lines show kobo unit prices. */
export function LinesTable({ lines }: { lines: InvoiceLine[] }) {
  const dollars = lines.some((l) => l.amount_usd != null);
  return (
    <Table
      head={[
        "Description",
        "Quantity",
        dollars ? "Unit price" : "Unit (kobo)",
        "Amount",
      ]}
    >
      {lines.map((l, i) => (
        <tr key={i} data-testid="invoice-line" data-kind={l.kind}>
          <td className="px-3 py-2">{l.description}</td>
          <td className="px-3 py-2 text-right tabular-nums text-muted">
            {l.quantity}
          </td>
          <td className="px-3 py-2 text-right tabular-nums text-muted">
            {l.amount_usd != null && l.unit_price_usd != null
              ? usdUnit(Number(l.unit_price_usd))
              : l.unit_price}
          </td>
          <td className="px-3 py-2 text-right tabular-nums">
            {l.amount_usd != null ? (
              <>
                {usd(l.amount_usd)}
                <span className="block text-[12px] text-muted">
                  {naira(l.amount)}
                </span>
              </>
            ) : (
              naira(l.amount)
            )}
          </td>
        </tr>
      ))}
    </Table>
  );
}

/** An invoice's total in dollars and naira; a draft's naira is an estimate
 * at the indicative rate until the invoice is issued. */
export function InvoiceTotal({ inv }: { inv: Invoice }) {
  if (inv.usd_total_minor == null) return <>{naira(inv.total_minor)}</>;
  return (
    <>
      {usd(inv.usd_total_minor)} ·{" "}
      {inv.status === "draft" ? (
        <span className="text-muted">
          ≈ {naira(inv.total_minor)} (estimate)
        </span>
      ) : (
        naira(inv.total_minor)
      )}
    </>
  );
}

/** "12 Oct 2026". */
function day(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { dateStyle: "medium" });
}

export function InvoiceStatus({ inv }: { inv: Invoice }) {
  const s = INVOICE_STATUS[inv.status] ?? {
    label: inv.status,
    tone: "muted" as const,
  };
  return (
    <span className="inline-flex items-center gap-1">
      <Badge tone={s.tone}>{s.label}</Badge>
      {inv.held && <Badge tone="warn">Held</Badge>}
    </span>
  );
}

/** An organisation's billing: plan, forecast, spend controls, details,
 * contacts and invoices (V3 §3). */
export function BillingPage() {
  const search = useSearch({ strict: false }) as {
    org?: string;
    invoice?: string;
  };
  useEffect(() => {
    if (search.org) setCurrentOrg(search.org);
  }, [search.org]);
  const { org } = useCurrentOrg();
  const allowed = canSeeBilling(org?.role);
  const acct = useQuery({
    queryKey: ["billing", org?.id],
    queryFn: () => api.billing(org!.id),
    enabled: !!org && allowed,
  });
  const forecast = useQuery({
    queryKey: ["forecast", org?.id],
    queryFn: () => api.forecast(org!.id),
    enabled: !!org && allowed,
    refetchInterval: 60_000,
  });
  const invoices = useQuery({
    queryKey: ["invoices", org?.id],
    queryFn: () => api.invoices(org!.id),
    enabled: !!org && allowed,
  });
  const [openInvoice, setOpenInvoice] = useState<string | undefined>(
    search.invoice,
  );
  useEffect(() => setOpenInvoice(search.invoice), [search.invoice]);

  if (!org) return <PageSkeleton />;
  if (!allowed)
    return (
      <EmptyState title="Only owners and billing members see billing">
        Ask an owner to give you the billing role.
      </EmptyState>
    );
  if (acct.isError) return <Alert>{errorMessage(acct.error)}</Alert>;
  if (!acct.data) return <PageSkeleton />;
  const a = acct.data;
  const f = forecast.data;

  return (
    <Page
      title="Billing"
      description={`${org.name}'s plan, spend and invoices. Amounts are before VAT unless they say otherwise.`}
      testId="billing"
    >
      {org.role === "owner" && a.plan !== "free" && (
        <LegalBanner orgId={org.id} />
      )}
      <StandingBanner a={a} />
      {a.capped && (
        <Alert tone="warn" title="Spend cap reached">
          New branches, dedicated instances and HA are paused, webhook
          deliveries queue and scheduled jobs are skipped until you raise the
          cap or next month starts. Databases stay connected.
        </Alert>
      )}
      <div className="grid gap-4 lg:grid-cols-2">
        <PlanPanel org={org.id} a={a} owner={org.role === "owner"} />
        <Panel
          title={f ? `${periodLabel(f.month)} so far` : "This month"}
          testId="forecast"
        >
          {f ? (
            <div className="flex flex-col gap-4">
              <div className="grid grid-cols-3 gap-3">
                <Stat
                  label="Forecast"
                  value={naira(f.spend_minor)}
                  testId="forecast-spend"
                />
                <Stat
                  label="Usage charges"
                  value={naira(f.usage_minor)}
                  hint="overage, dedicated, add-ons"
                />
                <Stat
                  label="Month elapsed"
                  value={`${Math.round(Number(f.elapsed) * 100)}%`}
                />
              </div>
              {(f.budget_minor != null || f.spend_cap_minor != null) && (
                <p className="text-[13px] text-muted">
                  {f.budget_minor != null && (
                    <>Budget {naira(f.budget_minor)}. </>
                  )}
                  {f.spend_cap_minor != null && (
                    <>Spend cap {naira(f.spend_cap_minor)} on usage charges.</>
                  )}
                </p>
              )}
              {f.by_service.length > 0 && (
                <Table head={["Service", "So far", "Projected"]}>
                  {f.by_service.map((b) => (
                    <tr
                      key={b.service}
                      data-testid="service-spend"
                      data-service={b.service}
                    >
                      <td className="px-3 py-2">
                        {SERVICE_LABELS[b.service] ?? b.service}
                      </td>
                      <td className="px-3 py-2 text-right tabular-nums text-muted">
                        {naira(b.so_far_minor)}
                      </td>
                      <td className="px-3 py-2 text-right tabular-nums">
                        {naira(b.projected_minor)}
                      </td>
                    </tr>
                  ))}
                </Table>
              )}
              {f.so_far.length > 0 && (
                <details>
                  <summary className="cursor-pointer text-[13px] text-muted">
                    The next invoice so far ({f.so_far.length} lines)
                  </summary>
                  <LinesTable lines={f.so_far} />
                </details>
              )}
            </div>
          ) : (
            <p className="text-muted">Working out the forecast…</p>
          )}
        </Panel>
      </div>
      <WalletPanel org={org.id} orgName={org.name} a={a} />
      <SpendControls org={org.id} a={a} />
      <DetailsPanel org={org.id} a={a} />
      <ContactsPanel org={org.id} />
      <Panel title="Invoices" testId="invoices">
        {invoices.data && invoices.data.items.length === 0 ? (
          <p className="text-muted">
            No invoices yet. Each month&apos;s invoice is issued on the 1st and
            emailed to the billing contacts.
          </p>
        ) : (
          <Table head={["Number", "Period", "Total", "Status", "Due", ""]}>
            {(invoices.data?.items ?? []).map((inv) => (
              <tr key={inv.id} data-testid="invoice-row">
                <td className="px-3 py-2 font-mono text-xs">
                  <button
                    className="text-accent-text hover:underline"
                    onClick={() => setOpenInvoice(inv.id)}
                  >
                    {inv.number}
                  </button>
                </td>
                <td className="px-3 py-2">
                  {periodLabel(inv.period_start.slice(0, 7))}
                </td>
                <td className="px-3 py-2 tabular-nums">
                  <InvoiceTotal inv={inv} />
                </td>
                <td className="px-3 py-2">
                  <InvoiceStatus inv={inv} />
                </td>
                <td className="px-3 py-2 text-muted">
                  {inv.total_minor > 0 ? formatDate(inv.due_at) : "—"}
                </td>
                <td className="px-3 py-2 text-right">
                  <a
                    className="inline-flex items-center gap-1 text-accent-text hover:underline"
                    href={api.invoicePdfUrl(org.id, inv.id)}
                  >
                    <Download className="h-3.5 w-3.5" /> PDF
                  </a>
                </td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
      <PaymentsPanel org={org.id} />
      <InvoicePanel
        org={org.id}
        id={openInvoice}
        onClose={() => setOpenInvoice(undefined)}
      />
    </Page>
  );
}

/** A plan fee in the price book's currency; dollars add the naira at the
 * indicative rate: "$10.00 ≈ ₦15,150". */
function planFee(
  p: PlanOption,
  k: "monthly_minor" | "annual_minor",
  fx: BillingAccount["fx"],
) {
  if (p.currency !== "USD") return naira(p[k]);
  const n = approxNaira(p[k], fx);
  return n ? `${usd(p[k])} ${n}` : usd(p[k]);
}

function PlanPanel({
  org,
  a,
  owner,
}: {
  org: string;
  a: BillingAccount;
  owner: boolean;
}) {
  const qc = useQueryClient();
  const legal = useQuery({
    queryKey: ["org", org, "legal"],
    queryFn: () => api.orgLegal(org),
    enabled: owner,
  });
  const [acceptLegal, setAcceptLegal] = useState(false);
  const [open, setOpen] = useState(false);
  const [plan, setPlan] = useState(a.plan);
  const [term, setTerm] = useState<"monthly" | "annual">(a.term);
  const [immediately, setImmediately] = useState(false);
  const [preview, setPreview] = useState<PlanChange | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const option = a.plans.find((p) => p.id === plan);

  useEffect(() => {
    if (!open || (plan === a.plan && term === a.term && !a.pending_change)) {
      setPreview(null);
      return;
    }
    let live = true;
    setErr(null);
    api
      .changePlan(org, { plan, term, immediately, dry_run: true })
      .then((p) => live && setPreview(p))
      .catch((e) => live && (setPreview(null), setErr(errorMessage(e))));
    return () => {
      live = false;
    };
  }, [open, org, plan, term, immediately, a.plan, a.term, a.pending_change]);

  const apply = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.changePlan(org, {
        plan,
        term,
        immediately,
        accept_legal: acceptLegal || undefined,
      });
      setOpen(false);
      if (acceptLegal)
        await qc.invalidateQueries({ queryKey: ["org", org, "legal"] });
      await qc.invalidateQueries({ queryKey: ["billing", org] });
      await qc.invalidateQueries({ queryKey: ["forecast", org] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel
      title="Plan"
      testId="plan"
      actions={<Button onClick={() => setOpen(true)}>Change plan…</Button>}
    >
      <KeyValues
        items={[
          ["Plan", <span data-testid="plan-name">{a.plan_name}</span>],
          [
            "Term",
            a.term === "annual" && a.term_ends_at
              ? `Annual, renews ${formatDate(a.term_ends_at)}`
              : "Monthly",
          ],
          ["Billing", `Invoices due in ${a.payment_terms_days} days`],
          [
            "Price book",
            `Version ${a.price_book_version}${a.grandfathered ? " (kept for you)" : ""}${a.price_currency === "USD" ? ", prices in US dollars" : ""}`,
          ],
        ]}
      />
      {a.pending_change && (
        <p className="mt-3 text-[13px] text-muted" data-testid="pending-change">
          Changes to{" "}
          {a.plans.find((p) => p.id === a.pending_change!.to_plan)?.name ??
            a.pending_change.to_plan}{" "}
          ({a.pending_change.to_term}) on{" "}
          {formatDate(a.pending_change.effective_at)}.
        </p>
      )}
      <SidePanel
        open={open}
        onOpenChange={setOpen}
        title="Change plan"
        description="Upgrades start now and are charged by the day for the rest of the month; downgrades start next month unless you choose now."
        footer={
          <>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button
              variant="primary"
              busy={busy}
              disabled={!preview}
              onClick={apply}
            >
              {preview?.applied
                ? "Change plan now"
                : preview
                  ? `Change on ${formatDate(preview.effective_at)}`
                  : "Change plan"}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4 text-sm">
          <div className="grid gap-2">
            {a.plans.map((p) => (
              <label
                key={p.id}
                className="flex cursor-pointer items-center justify-between rounded-md border border-line px-3 py-2 has-[:checked]:border-accent"
              >
                <span className="flex items-center gap-2">
                  <input
                    type="radio"
                    name="plan"
                    value={p.id}
                    checked={plan === p.id}
                    onChange={() => setPlan(p.id)}
                  />
                  <span className="font-medium">{p.name}</span>
                  {p.id === a.plan && <Badge tone="muted">Current</Badge>}
                </span>
                <span className="text-right text-muted">
                  {p.monthly_minor ? (
                    <>
                      {planFee(p, "monthly_minor", a.fx)} / month
                    </>
                  ) : (
                    "Free"
                  )}
                  {p.annual_minor ? (
                    <>
                      {" · "}
                      {planFee(p, "annual_minor", a.fx)} / year
                    </>
                  ) : null}
                </span>
              </label>
            ))}
          </div>
          {!!option?.annual_minor && (
            <Field label="Term">
              {(id) => (
                <Select
                  id={id}
                  value={term}
                  onChange={(e) =>
                    setTerm(e.target.value as "monthly" | "annual")
                  }
                >
                  <option value="monthly">Monthly</option>
                  <option value="annual">Annual, paid in advance</option>
                </Select>
              )}
            </Field>
          )}
          {owner && plan !== "free" && legal.data?.outstanding && (
            <label
              className="flex items-start gap-2"
              data-testid="accept-legal"
            >
              <input
                type="checkbox"
                className="mt-1"
                checked={acceptLegal}
                onChange={(e) => setAcceptLegal(e.target.checked)}
              />
              <span>
                Accept the{" "}
                {legal.data.items
                  .filter((d) => !d.accepted_at)
                  .map((d) => d.document.title.toLowerCase())
                  .join(", ")}{" "}
                for the organisation (on the{" "}
                <Link to="/org/legal" className="underline">
                  Legal page
                </Link>
                )
              </span>
            </label>
          )}
          {preview && !preview.upgrade && (
            <label className="flex items-center gap-2">
              <input
                type="checkbox"
                checked={immediately}
                onChange={(e) => setImmediately(e.target.checked)}
              />
              Change now, with a credit for the unused part of the current plan
            </label>
          )}
          {preview && (
            <div data-testid="plan-preview" className="flex flex-col gap-2">
              {preview.lines.length > 0 ? (
                <>
                  <LinesTable lines={preview.lines} />
                  <p className="text-right font-medium" data-testid="plan-total">
                    On the next invoice:{" "}
                    {preview.total_usd_minor != null
                      ? `${usd(preview.total_usd_minor)} (${naira(preview.total_minor)})`
                      : naira(preview.total_minor)}{" "}
                    + VAT
                  </p>
                  {preview.total_usd_minor != null && preview.fx && (
                    <p className="text-right text-[12px] text-muted">
                      At {rate(preview.fx)}, the rate today; the invoice uses
                      the rate when it is issued.
                    </p>
                  )}
                </>
              ) : (
                <p className="text-muted">
                  {preview.from_plan === preview.to_plan &&
                  preview.from_term === preview.to_term
                    ? "Keeps the current plan and cancels the scheduled change."
                    : `Nothing to prorate: the new plan starts on ${formatDate(preview.effective_at)}.`}
                </p>
              )}
            </div>
          )}
          {err && <Alert>{err}</Alert>}
        </div>
      </SidePanel>
    </Panel>
  );
}

function SpendControls({ org, a }: { org: string; a: BillingAccount }) {
  const qc = useQueryClient();
  const [budget, setBudget] = useState(
    a.budget_minor != null ? String(a.budget_minor / 100) : "",
  );
  const [cap, setCap] = useState(
    a.spend_cap_minor != null ? String(a.spend_cap_minor / 100) : "",
  );
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const save = async () => {
    const b = parseNaira(budget);
    const c = parseNaira(cap);
    if ((budget.trim() && b == null) || (cap.trim() && c == null)) {
      setMsg({
        ok: false,
        text: "Amounts are in naira, e.g. 50,000 or 50000.00.",
      });
      return;
    }
    setBusy(true);
    setMsg(null);
    try {
      await api.updateBilling(org, {
        ...details(a),
        budget_minor: b,
        spend_cap_minor: c,
      });
      await qc.invalidateQueries({ queryKey: ["billing", org] });
      await qc.invalidateQueries({ queryKey: ["forecast", org] });
      setMsg({ ok: true, text: "Saved." });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Spend controls"
      description="Alerts at 50%, 80% and 100% of the budget. At the spend cap, new billable resources pause; nothing running stops and no data is deleted."
      testId="spend-controls"
      footer={
        <Button variant="primary" busy={busy} onClick={save}>
          Save
        </Button>
      }
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Monthly budget (₦)" hint="Leave empty for none.">
          {(id) => (
            <Input
              id={id}
              inputMode="decimal"
              value={budget}
              onChange={(e) => setBudget(e.target.value)}
              placeholder="50,000"
            />
          )}
        </Field>
        <Field
          label="Spend cap on usage charges (₦)"
          hint="Leave empty for none."
        >
          {(id) => (
            <Input
              id={id}
              inputMode="decimal"
              value={cap}
              onChange={(e) => setCap(e.target.value)}
              placeholder="100,000"
            />
          )}
        </Field>
      </div>
      {msg && (
        <div className="mt-3">
          <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>
        </div>
      )}
    </Panel>
  );
}

function details(a: BillingAccount) {
  return {
    legal_name: a.legal_name ?? null,
    address: a.address ?? null,
    tin: a.tin ?? null,
    vat_registered: a.vat_registered,
    deducts_wht: a.deducts_wht,
    budget_minor: a.budget_minor ?? null,
    spend_cap_minor: a.spend_cap_minor ?? null,
  };
}

function DetailsPanel({ org, a }: { org: string; a: BillingAccount }) {
  const qc = useQueryClient();
  const [d, setD] = useState(details(a));
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      await api.updateBilling(org, {
        ...d,
        budget_minor: a.budget_minor ?? null,
        spend_cap_minor: a.spend_cap_minor ?? null,
      });
      await qc.invalidateQueries({ queryKey: ["billing", org] });
      setMsg({
        ok: true,
        text: "Saved. New invoices will show these details.",
      });
    } catch (e) {
      setMsg({ ok: false, text: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel
      title="Business details"
      description="Shown on invoices, for B2B and tax purposes."
      testId="business-details"
      footer={
        <Button variant="primary" busy={busy} onClick={save}>
          Save
        </Button>
      }
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Legal name">
          {(id) => (
            <Input
              id={id}
              value={d.legal_name ?? ""}
              onChange={(e) => setD({ ...d, legal_name: e.target.value })}
            />
          )}
        </Field>
        <Field label="TIN">
          {(id) => (
            <Input
              id={id}
              value={d.tin ?? ""}
              onChange={(e) => setD({ ...d, tin: e.target.value })}
            />
          )}
        </Field>
        <div className="sm:col-span-2">
          <Field label="Address">
            {(id) => (
              <Input
                id={id}
                value={d.address ?? ""}
                onChange={(e) => setD({ ...d, address: e.target.value })}
              />
            )}
          </Field>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={d.vat_registered}
            onChange={(e) => setD({ ...d, vat_registered: e.target.checked })}
          />
          VAT registered
        </label>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={d.deducts_wht}
            onChange={(e) => setD({ ...d, deducts_wht: e.target.checked })}
          />
          We deduct withholding tax (invoices show the expected WHT)
        </label>
      </div>
      {msg && (
        <div className="mt-3">
          <Alert tone={msg.ok ? "ok" : "danger"}>{msg.text}</Alert>
        </div>
      )}
    </Panel>
  );
}

function ContactsPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["billing-contacts", org],
    queryFn: () => api.billingContacts(org),
  });
  const [email, setEmail] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const refresh = () =>
    qc.invalidateQueries({ queryKey: ["billing-contacts", org] });
  const add = async () => {
    setErr(null);
    try {
      await api.addBillingContact(org, { email });
      setEmail("");
      await refresh();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };
  const items = q.data?.items ?? [];
  return (
    <Panel
      title="Billing contacts"
      description="Invoices and payment emails go here. With none, they go to the owners and billing members. On a paid plan, contacts with status page emails on (and the owners) are emailed about incidents affecting the organisation's projects; each email has an unsubscribe link. A contact who isn't a member gets a link to confirm the address; until then, their email to support isn't filed under this organisation."
      testId="billing-contacts"
    >
      <div className="flex flex-col gap-3">
        {items.map((c) => (
          <div
            key={c.email}
            className="flex items-center justify-between text-sm"
            data-testid="billing-contact"
          >
            <span className="flex items-center gap-2">
              {c.email}
              {c.verified === false && (
                <>
                  <Badge tone="warn">unconfirmed</Badge>
                  <Button
                    size="small"
                    variant="ghost"
                    onClick={async () => {
                      setErr(null);
                      try {
                        await api.addBillingContact(org, { email: c.email });
                      } catch (x) {
                        setErr(errorMessage(x));
                      }
                    }}
                  >
                    Send the link again
                  </Button>
                </>
              )}
            </span>
            <div className="flex items-center gap-3">
              <label className="flex items-center gap-2 text-xs text-fg-muted">
                <input
                  type="checkbox"
                  checked={c.status_emails ?? true}
                  onChange={async (e) => {
                    setErr(null);
                    try {
                      await api.updateBillingContact(
                        org,
                        c.email,
                        e.target.checked,
                      );
                      await refresh();
                    } catch (x) {
                      setErr(errorMessage(x));
                    }
                  }}
                  data-testid="status-emails"
                />
                Status page emails
              </label>
              <Button
                size="small"
                onClick={async () => {
                  await api.removeBillingContact(org, c.email);
                  await refresh();
                }}
              >
                Remove
              </Button>
            </div>
          </div>
        ))}
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void add();
          }}
        >
          <Input
            type="email"
            required
            placeholder="accounts@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            aria-label="Contact email"
          />
          <Button type="submit">Add</Button>
        </form>
        {err && <Alert>{err}</Alert>}
      </div>
    </Panel>
  );
}

function InvoicePanel({
  org,
  id,
  onClose,
}: {
  org: string;
  id?: string;
  onClose: () => void;
}) {
  const q = useQuery({
    queryKey: ["invoice", org, id],
    queryFn: () => api.invoice(org, id!),
    enabled: !!id,
  });
  const d = q.data;
  return (
    <SidePanel
      open={!!id}
      onOpenChange={(o) => !o && onClose()}
      size="large"
      title={d?.invoice.number ?? "Invoice"}
      testId="invoice-panel"
      footer={
        id && (
          <a href={api.invoicePdfUrl(org, id)}>
            <Button>
              <Download className="h-4 w-4" /> Download PDF
            </Button>
          </a>
        )
      }
    >
      {q.isError && <Alert>{errorMessage(q.error)}</Alert>}
      {d && (
        <div className="flex flex-col gap-4">
          <InvoicePay org={org} inv={d.invoice} />
          <InvoiceBody d={d} />
        </div>
      )}
    </SidePanel>
  );
}

export function InvoiceBody({
  d,
}: {
  d: NonNullable<Awaited<ReturnType<typeof api.invoice>>>;
}) {
  const inv = d.invoice;
  return (
    <div className="flex flex-col gap-4 text-sm">
      <KeyValues
        items={[
          [
            "Usage period",
            `${formatDate(inv.period_start)} – ${formatDate(inv.period_end)}`,
          ],
          ["Status", <InvoiceStatus inv={inv} />],
          ...(inv.issued_at
            ? ([["Issued", formatDate(inv.issued_at)]] as [string, string][])
            : []),
          ...(inv.due_at && inv.total_minor > 0
            ? ([["Due", formatDate(inv.due_at)]] as [string, string][])
            : []),
        ]}
      />
      <LinesTable lines={d.lines} />
      {inv.usd_total_minor != null && (
        <div
          className="ml-auto grid w-72 grid-cols-2 gap-1 tabular-nums"
          data-testid="invoice-usd"
        >
          <span className="text-muted">Subtotal</span>
          <span className="text-right">{usd(inv.usd_subtotal_minor ?? 0)}</span>
          <span className="text-muted">VAT</span>
          <span className="text-right">{usd(inv.usd_vat_minor ?? 0)}</span>
          <span className="font-medium">Total</span>
          <span
            className="text-right font-medium"
            data-testid="invoice-usd-total"
          >
            {usd(inv.usd_total_minor)}
          </span>
        </div>
      )}
      {inv.usd_total_minor != null && inv.fx && (
        <p className="text-right text-[13px] text-muted" data-testid="invoice-fx">
          {inv.status === "draft"
            ? `Estimated at ${rate(inv.fx)}, the indicative rate; the rate is fixed when the invoice is issued`
            : `Converted at ${rate(inv.fx)} on ${day(inv.issued_at ?? inv.fx.quoted_at)}`}{" "}
          (market {nairaRate(inv.fx.market_rate)}, spread{" "}
          {bps(inv.fx.spread_bps)}).
        </p>
      )}
      <div className="ml-auto grid w-72 grid-cols-2 gap-1 tabular-nums">
        <span className="text-muted">Subtotal</span>
        <span className="text-right">{naira(inv.subtotal_minor)}</span>
        <span className="text-muted">
          VAT at {(Number(inv.vat_rate) * 100).toFixed(1).replace(/\.0$/, "")}%
        </span>
        <span className="text-right">{naira(inv.vat_minor)}</span>
        <span className="font-medium">
          {inv.usd_total_minor != null ? "Total in naira" : "Total"}
        </span>
        <span className="text-right font-medium" data-testid="invoice-total">
          {naira(inv.total_minor)}
        </span>
        {inv.wht_expected_minor > 0 && (
          <>
            <span className="text-muted">WHT you may deduct</span>
            <span className="text-right">{naira(inv.wht_expected_minor)}</span>
          </>
        )}
      </div>
      {d.credit_notes.length > 0 && (
        <Table head={["Credit note", "Reason", "Amount"]}>
          {d.credit_notes.map((c) => (
            <tr key={c.id}>
              <td className="px-3 py-2 font-mono text-xs">{c.number}</td>
              <td className="px-3 py-2">{c.reason}</td>
              <td className="px-3 py-2 text-right tabular-nums">
                -{naira(c.amount_minor + c.vat_minor)}
              </td>
            </tr>
          ))}
        </Table>
      )}
    </div>
  );
}
