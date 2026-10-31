import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { Link } from "react-router";

import { unwrap, type Status } from "@/api/client";
import { useApi } from "@/api/context";
import { DecisionButtons } from "@/components/actions";
import { Button, EmptyState, ErrorState, Loading, PageHeader, StatusBadge } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatRelative } from "@/lib/format";

export function ApprovalsPage() {
  const { t, locale } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["approvals"],
    queryFn: async (): Promise<Status[]> => unwrap(await api.GET("/api/v1/approvals")).approvals,
    refetchInterval: 15_000,
  });
  return (
    <>
      <PageHeader
        title={t("approvals.title")}
        lead={t("approvals.lead")}
        actions={
          <Button icon={RefreshCw} onClick={() => void q.refetch()} busy={q.isRefetching}>
            {t("common.refresh")}
          </Button>
        }
      />
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.data.length === 0 ? (
        <EmptyState>{t("approvals.empty")}</EmptyState>
      ) : (
        <ul className="flex flex-col gap-3">
          {q.data.map((d) => {
            const a = d.rollout?.approval ?? d.approval;
            const done = a?.decisions?.filter((x) => x.approve).length ?? 0;
            const after = d.rollout?.stages.filter((s) => s.state === "succeeded").at(-1)?.name;
            return (
              <li key={d.id} className="card flex flex-wrap items-center gap-4 p-4">
                <div className="min-w-0 flex-1">
                  <h2 className="text-lg font-semibold">
                    <Link to={`/deployments/${encodeURIComponent(d.id)}`} className="hover:text-accent hover:underline">
                      {d.application} <span aria-hidden>→</span>
                      <span className="sr-only"> / </span> {d.environment}{" "}
                      <span className="font-mono text-base text-muted">{d.version}</span>
                    </Link>
                  </h2>
                  <p className="text-sm text-muted">
                    {d.rollout ? t("approvals.betweenWaves", { stage: after ?? "—" }) : t("approvals.beforeDeploy")}
                    {" · "}
                    {t("approvals.requested", { who: d.requested_by ?? "—", time: formatRelative(d.created_at, locale) })}
                  </p>
                  {a && (
                    <p className="text-sm text-muted">
                      {t("detail.approval.required", { done, required: a.required })} ·{" "}
                      {t("detail.approval.expires", { time: formatRelative(a.expires_at, locale) })}
                    </p>
                  )}
                </div>
                <StatusBadge state={d.state} />
                <div className="flex flex-wrap items-center gap-2">
                  <DecisionButtons dep={d} />
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </>
  );
}
