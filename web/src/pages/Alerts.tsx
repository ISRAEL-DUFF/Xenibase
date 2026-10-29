import { Link } from "@tanstack/react-router";
import { BellOff } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, errorMessage, type AlertItem } from "../api/client";
import { Alert, Badge, Panel, EmptyState, PageHeading, Spinner, Table, cx } from "../components/ui";
import { formatDate, relativeTime } from "../lib/format";

const kindLabel: Record<string, string> = {
  backup_failed: "Backup failed",
  backup_overdue: "Backup overdue",
  restore_test_failed: "Restore test failed",
  node_disk: "Node disk",
  node_unreachable: "Node unreachable",
  project_disk: "Project disk",
  pooler_down: "Pooler down",
  pooler_host_not_ready: "Pooler host not ready",
  pooler_split_brain: "Pooler split brain",
  pooler_allowlist_off: "Pooler ignores IP allowlists",
  waker_down: "Waker down",
  failure_domain: "Shared failure domain",
  isolation_check_failed: "Isolation check",
  capacity_proposal: "Capacity proposal",
  capacity_failed: "Provisioning failed",
};

function TargetLink({ a }: { a: AlertItem }) {
  if (a.target_type === "project" && a.target_id.length === 36) {
    return (
      <Link to="/projects/$id" params={{ id: a.target_id }} className="hover:underline">
        {a.target_name}
      </Link>
    );
  }
  if (a.target_type === "node") {
    return (
      <Link to="/nodes/$id" params={{ id: a.target_id }} className="hover:underline">
        {a.target_name}
      </Link>
    );
  }
  return <>{a.target_name}</>;
}

/** Alerts (spec §8.8), firing first. */
export function AlertsPage() {
  const [filter, setFilter] = useState<"" | "firing" | "resolved">("");
  const q = useQuery({ queryKey: ["alerts", filter], queryFn: () => api.alerts(filter || undefined), refetchInterval: 15_000 });
  return (
    <>
      <PageHeading
        title="Alerts"
        description="Checked every 30 seconds; delivered to the webhook and email set in Settings."
        actions={
          <div role="radiogroup" aria-label="Filter" className="inline-flex rounded-md border border-line">
            {(["", "firing", "resolved"] as const).map((f) => (
              <button
                key={f || "all"}
                type="button"
                role="radio"
                aria-checked={filter === f}
                onClick={() => setFilter(f)}
                className={cx("px-2.5 py-1 text-xs", filter === f ? "bg-accent text-accent-fg" : "text-muted hover:text-fg")}
              >
                {f || "all"}
              </button>
            ))}
          </div>
        }
      />
      {q.isPending ? (
        <Spinner />
      ) : q.isError ? (
        <Alert>{errorMessage(q.error)}</Alert>
      ) : q.data.items.length === 0 ? (
        <EmptyState title={filter === "resolved" ? "No resolved alerts" : "Nothing is wrong"} icon={<BellOff />}>
          No alerts {filter ? `(${filter})` : "yet"}.
        </EmptyState>
      ) : (
        <Panel>
          <Table head={["", "Alert", "Target", "Started", "Status", "Notified"]}>
            {q.data.items.map((a) => (
              <tr key={a.id} data-testid="alert-row">
                <td className="px-3 py-2">
                  <Badge tone={a.status === "resolved" ? "muted" : a.severity === "critical" ? "danger" : "warn"}>{a.severity}</Badge>
                </td>
                <td className="px-3 py-2">
                  <div className="font-medium">{kindLabel[a.kind] ?? a.kind}</div>
                  <div className="max-w-xl text-xs text-muted">{a.summary}</div>
                </td>
                <td className="px-3 py-2 text-sm">
                  <TargetLink a={a} />
                </td>
                <td className="px-3 py-2 text-xs text-muted" title={formatDate(a.started_at)}>
                  {relativeTime(a.started_at)}
                </td>
                <td className="px-3 py-2 text-xs">
                  {a.status === "firing" ? <Badge tone="danger">firing</Badge> : <span className="text-muted">resolved {relativeTime(a.resolved_at)}</span>}
                </td>
                <td className="px-3 py-2 text-xs text-muted">
                  {a.delivery_error ? (
                    <span className="text-danger-text" title={a.delivery_error}>
                      retrying
                    </span>
                  ) : a.notified_at ? (
                    "sent"
                  ) : (
                    "—"
                  )}
                </td>
              </tr>
            ))}
          </Table>
        </Panel>
      )}
    </>
  );
}
