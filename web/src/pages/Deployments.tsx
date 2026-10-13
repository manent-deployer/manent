import { useInfiniteQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { unwrap, type Status } from "@/api/client";
import { useApi } from "@/api/context";
import { Badge, Button, EmptyState, ErrorState, Field, Loading, PageHeader, StatusBadge, inputClass } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";
import { durationBetween, formatDateTime, formatDuration, formatRelative, shortId } from "@/lib/format";

const STATES = ["PENDING", "ENQUEUED", "DELAYED", "AWAITING_APPROVAL", "WAITING_DEPENDENCY", "SUCCESS", "ERROR", "CANCELLED", "REJECTED", "EXPIRED", "DEPENDENCY_FAILED"];
const PAGE = 50;

export function DeploymentsPage() {
  const { t, locale } = useI18n();
  const api = useApi();
  const [params, setParams] = useSearchParams();
  const filters = {
    application: params.get("application") ?? "",
    environment: params.get("environment") ?? "",
    state: params.get("state") ?? "",
  };
  const [draft, setDraft] = useState(filters);

  const q = useInfiniteQuery({
    queryKey: ["deployments", filters],
    initialPageParam: "",
    queryFn: async ({ pageParam }) => {
      const query: Record<string, string | number> = { limit: PAGE };
      for (const [k, v] of Object.entries(filters)) if (v) query[k] = v;
      if (pageParam) query.cursor = pageParam;
      return unwrap(await api.GET("/api/v1/deployments", { params: { query } }));
    },
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    refetchInterval: 15_000,
  });
  const rows: Status[] = q.data?.pages.flatMap((p) => p.deployments) ?? [];

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = new URLSearchParams();
    for (const [k, v] of Object.entries(draft)) if (v.trim()) next.set(k, v.trim());
    setParams(next);
  };
  const reset = () => {
    setDraft({ application: "", environment: "", state: "" });
    setParams(new URLSearchParams());
  };

  return (
    <>
      <PageHeader
        title={t("deployments.title")}
        lead={t("deployments.lead")}
        actions={
          <Button icon={RefreshCw} onClick={() => void q.refetch()} busy={q.isRefetching}>
            {t("common.refresh")}
          </Button>
        }
      />
      <form onSubmit={submit} className="card mb-4 grid gap-3 p-4 sm:grid-cols-[1fr_1fr_1fr_auto] sm:items-end" aria-label={t("common.filters")}>
        <Field label={t("deployments.filter.application")}>
          {(p) => <input {...p} className={inputClass} value={draft.application} onChange={(e) => setDraft({ ...draft, application: e.target.value })} />}
        </Field>
        <Field label={t("deployments.filter.environment")}>
          {(p) => <input {...p} className={inputClass} value={draft.environment} onChange={(e) => setDraft({ ...draft, environment: e.target.value })} />}
        </Field>
        <Field label={t("deployments.filter.state")}>
          {(p) => (
            <select {...p} className={inputClass} value={draft.state} onChange={(e) => setDraft({ ...draft, state: e.target.value })}>
              <option value="">{t("common.all")}</option>
              {STATES.map((s) => (
                <option key={s} value={s}>
                  {t(`state.${s}` as MessageKey)}
                </option>
              ))}
            </select>
          )}
        </Field>
        <div className="flex gap-2">
          <Button type="submit" variant="primary">
            {t("common.apply")}
          </Button>
          <Button variant="ghost" onClick={reset}>
            {t("common.reset")}
          </Button>
        </div>
      </form>

      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : rows.length === 0 ? (
        <EmptyState>{t("deployments.empty")}</EmptyState>
      ) : (
        <div className="card overflow-x-auto">
          <table className="w-full min-w-[56rem] text-left text-sm">
            <caption className="sr-only">{t("deployments.caption")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.deployment")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.version")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.kind")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.state")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.rollout")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.requestedBy")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.started")}</th>
                <th scope="col" className="px-4 py-3 font-medium">{t("deployments.col.duration")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.id} className="border-b border-border/40 last:border-0 hover:bg-raised/40">
                  <th scope="row" className="px-4 py-3 font-normal">
                    <Link to={`/deployments/${encodeURIComponent(d.id)}`} className="font-medium text-fg underline-offset-4 hover:text-accent hover:underline">
                      {d.application ?? "—"} <span aria-hidden>→</span>
                      <span className="sr-only"> / </span> {d.environment ?? "—"}
                    </Link>
                    <div className="font-mono text-xs text-muted">{shortId(d.id)}</div>
                  </th>
                  <td className="px-4 py-3 font-mono">{d.version || "—"}</td>
                  <td className="px-4 py-3">
                    <span className="flex flex-wrap gap-1">
                      {t(`kind.${d.kind ?? "deploy"}` as MessageKey)}
                      {d.dry_run && <Badge tone="violet">{t("deployments.dryRun")}</Badge>}
                    </span>
                  </td>
                  <td className="px-4 py-3">
                    <StatusBadge state={d.state} />
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">{d.rollout_progress ?? "—"}</td>
                  <td className="px-4 py-3 text-muted">{d.requested_by ?? "—"}</td>
                  <td className="px-4 py-3 text-muted">
                    <time dateTime={d.created_at} title={formatDateTime(d.created_at, locale)}>
                      {formatRelative(d.created_at, locale)}
                    </time>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">
                    {formatDuration(durationBetween(d.created_at, isRunning(d.state) ? undefined : d.updated_at))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="flex items-center justify-between gap-2 border-t border-border/50 px-4 py-3 text-sm text-muted">
            <span>{t("deployments.count", { count: rows.length })}</span>
            {q.hasNextPage && (
              <Button size="sm" onClick={() => void q.fetchNextPage()} busy={q.isFetchingNextPage}>
                {t("common.loadMore")}
              </Button>
            )}
          </div>
        </div>
      )}
    </>
  );
}

function isRunning(state: string) {
  return state === "PENDING" || state === "ENQUEUED" || state === "AWAITING_APPROVAL" || state === "DELAYED" || state === "WAITING_DEPENDENCY";
}
