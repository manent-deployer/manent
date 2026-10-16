import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, Rocket, Undo2 } from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { unwrap, type InventoryEntry } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { PromoteDialog, RollbackDialog } from "@/components/actions";
import { EmptyState, ErrorState, Field, Loading, PageHeader, StatusBadge, inputClass } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime, formatRelative } from "@/lib/format";
import { can, canInApp } from "@/lib/permissions";

type Open = { kind: "promote" | "rollback"; application: string; environment: string } | null;

function Cell({ e, onAction }: { e?: InventoryEntry; onAction: (o: Open) => void }) {
  const { t, locale } = useI18n();
  const me = useMe();
  if (!e) return <span className="text-sm text-muted">{t("inventory.notDeployed")}</span>;
  const cur = e.current;
  // The destination is chosen in the dialog: being able to release the application somewhere is enough.
  const canPromote = cur && canInApp(me.data, "deployment.submit", e.application);
  const canRollback = cur && can(me.data, "deployment.rollback", e.application, e.environment);
  return (
    <div className="flex flex-col gap-1.5">
      {cur ? (
        <Link
          to={`/deployments/${encodeURIComponent(cur.id)}`}
          className="font-mono text-base font-semibold text-fg hover:text-accent hover:underline"
          title={formatDateTime(cur.updated_at, locale)}
        >
          {cur.version || "—"}
        </Link>
      ) : (
        <span className="text-sm text-muted">{t("inventory.notDeployed")}</span>
      )}
      {cur && (
        <span className="text-xs text-muted">
          <time dateTime={cur.updated_at}>{formatRelative(cur.updated_at, locale)}</time>
        </span>
      )}
      {e.drift && (
        <span className="flex items-start gap-1 text-xs text-warn">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          {t("inventory.drift")}
        </span>
      )}
      {e.last_failure && (
        <Link to={`/deployments/${encodeURIComponent(e.last_failure.id)}`} className="flex flex-wrap items-center gap-1 text-xs text-muted hover:text-fg">
          <StatusBadge state={e.last_failure.state} />
          {t("inventory.lastFailure", { version: e.last_failure.version ?? "", time: formatRelative(e.last_failure.updated_at, locale) })}
        </Link>
      )}
      {(canPromote || canRollback) && (
        <div className="mt-1 flex flex-wrap gap-1" role="group" aria-label={t("inventory.actionsFor", { application: e.application, environment: e.environment })}>
          {canPromote && (
            <button
              type="button"
              onClick={() => onAction({ kind: "promote", application: e.application, environment: e.environment })}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs hover:border-accent hover:text-accent"
            >
              <Rocket className="h-3.5 w-3.5" aria-hidden />
              {t("actions.promote")}
            </button>
          )}
          {canRollback && (
            <button
              type="button"
              onClick={() => onAction({ kind: "rollback", application: e.application, environment: e.environment })}
              className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs hover:border-warn hover:text-warn"
            >
              <Undo2 className="h-3.5 w-3.5" aria-hidden />
              {t("actions.rollback")}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export function InventoryPage() {
  const { t } = useI18n();
  const api = useApi();
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<Open>(null);
  const q = useQuery({
    queryKey: ["inventory"],
    queryFn: async () => unwrap(await api.GET("/api/v1/inventory")).inventory,
    refetchInterval: 30_000,
  });
  const { apps, envs, byKey } = useMemo(() => {
    const entries = q.data ?? [];
    const envs = [...new Set(entries.map((e) => e.environment))].sort(envOrder);
    const apps = [...new Set(entries.map((e) => e.application))].sort();
    const byKey = new Map(entries.map((e) => [`${e.application}/${e.environment}`, e]));
    return { apps, envs, byKey };
  }, [q.data]);
  const shown = apps.filter((a) => a.toLowerCase().includes(filter.trim().toLowerCase()));

  return (
    <>
      <PageHeader title={t("inventory.title")} lead={t("inventory.lead")} />
      <div className="mb-4 max-w-sm">
        <Field label={t("inventory.filter")}>
          {(p) => <input {...p} type="search" className={inputClass} value={filter} onChange={(e) => setFilter(e.target.value)} />}
        </Field>
      </div>
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : shown.length === 0 ? (
        <EmptyState>{t("inventory.empty")}</EmptyState>
      ) : (
        <div className="card overflow-x-auto">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">{t("inventory.caption")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className="px-4 py-3 font-medium">
                  {t("inventory.application")}
                </th>
                {envs.map((env) => (
                  <th key={env} scope="col" className="px-4 py-3 font-mono font-medium normal-case">
                    {env}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {shown.map((app) => (
                <tr key={app} className="border-b border-border/40 align-top last:border-0">
                  <th scope="row" className="px-4 py-3 font-medium">
                    {app}
                  </th>
                  {envs.map((env) => (
                    <td key={env} className="px-4 py-3">
                      <Cell e={byKey.get(`${app}/${env}`)} onAction={setOpen} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {open?.kind === "promote" && (
        <PromoteDialog application={open.application} from={open.environment} environments={envs} onClose={() => setOpen(null)} />
      )}
      {open?.kind === "rollback" && <RollbackDialog application={open.application} environment={open.environment} onClose={() => setOpen(null)} />}
    </>
  );
}

// Usual environment order, then alphabetical: dev, test, qa, staging, preprod, prod.
const ORDER = ["dev", "test", "qa", "staging", "preprod", "prod"];
function envOrder(a: string, b: string) {
  const rank = (e: string) => {
    const i = ORDER.findIndex((o) => e === o || e.startsWith(o + "-"));
    return i < 0 ? ORDER.length : i;
  };
  return rank(a) - rank(b) || a.localeCompare(b);
}
