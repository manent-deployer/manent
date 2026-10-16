import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, CircleSlash, RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import { ApiError, TERMINAL, unwrap, type ReleaseStatus } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { useToast } from "@/components/Toaster";
import { Badge, Button, Dialog, EmptyState, ErrorState, Loading, PageHeader, Section, StatusBadge } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";
import { formatDateTime, formatRelative } from "@/lib/format";
import { can } from "@/lib/permissions";

// Releases change as deploys progress: frequent refresh while they are running.
const REFRESH = 5_000;

const th = "px-4 py-3 font-medium";
const td = "px-4 py-3";

function progress(r: ReleaseStatus) {
  return { done: r.deploys.filter((d) => d.state === "SUCCESS").length, total: r.deploys.length };
}

export function ReleasesPage() {
  const { t, locale } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["releases"],
    queryFn: async () => unwrap(await api.GET("/api/v1/releases", { params: { query: { limit: 50 } } })).releases,
    refetchInterval: REFRESH,
  });
  return (
    <>
      <PageHeader
        title={t("releases.title")}
        lead={t("releases.lead")}
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
        <EmptyState>{t("releases.empty")}</EmptyState>
      ) : (
        <div className="card overflow-x-auto">
          <table className="w-full min-w-[48rem] text-left text-sm">
            <caption className="sr-only">{t("releases.caption")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className={th}>{t("releases.col.release")}</th>
                <th scope="col" className={th}>{t("releases.col.state")}</th>
                <th scope="col" className={th}>{t("releases.col.progress")}</th>
                <th scope="col" className={th}>{t("releases.col.onFailure")}</th>
                <th scope="col" className={th}>{t("releases.col.requestedBy")}</th>
                <th scope="col" className={th}>{t("releases.col.created")}</th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((r) => (
                <tr key={r.id} className="border-b border-border/40 last:border-0 hover:bg-raised/40">
                  <th scope="row" className={`${td} font-normal`}>
                    <Link to={`/releases/${encodeURIComponent(r.id)}`} className="font-mono font-medium text-fg underline-offset-4 hover:text-accent hover:underline">
                      {r.id}
                    </Link>
                  </th>
                  <td className={td}>
                    <StatusBadge state={r.state} />
                  </td>
                  <td className={td}>{t("releases.progress", progress(r))}</td>
                  <td className={td}>{t(`releases.onFailure.${r.on_failure}` as MessageKey)}</td>
                  <td className={td}>{r.requested_by || "—"}</td>
                  <td className={td}>
                    <time dateTime={r.created_at} title={formatDateTime(r.created_at, locale)}>
                      {formatRelative(r.created_at, locale)}
                    </time>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function CancelRelease({ r }: { r: ReleaseStatus }) {
  const { t } = useI18n();
  const api = useApi();
  const me = useMe();
  const qc = useQueryClient();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const m = useMutation({
    mutationFn: async () => unwrap(await api.POST("/api/v1/releases/{id}/cancel", { params: { path: { id: r.id } } })),
    onSuccess: () => {
      toast("ok", t("releases.cancelled"));
      void qc.invalidateQueries({ queryKey: ["release", r.id] });
      setOpen(false);
    },
    onError: (e) => toast("error", t("actions.failed", { message: e instanceof Error ? e.message : String(e) })),
  });
  if (TERMINAL.has(r.state) || !r.deploys.every((d) => can(me.data, "deployment.cancel", d.application, d.environment))) return null;
  return (
    <>
      <Button variant="danger" icon={CircleSlash} onClick={() => setOpen(true)}>
        {t("releases.cancel")}
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t("releases.cancelTitle")}
        description={t("releases.cancelBody")}
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button variant="danger" busy={m.isPending} onClick={() => m.mutate()}>
              {t("releases.cancel")}
            </Button>
          </>
        }
      />
    </>
  );
}

export function ReleaseDetailPage() {
  const { id = "" } = useParams();
  const { t, locale } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["release", id],
    queryFn: async () => unwrap(await api.GET("/api/v1/releases/{id}", { params: { path: { id } } })),
    refetchInterval: (query) => (query.state.data && TERMINAL.has(query.state.data.state) ? false : REFRESH),
  });
  if (q.isPending) return <Loading />;
  if (q.isError) {
    const notFound = q.error instanceof ApiError && q.error.status === 404;
    return (
      <>
        <PageHeader title={notFound ? t("common.notFound") : t("app.error.title")} />
        <ErrorState error={q.error} onRetry={notFound ? undefined : () => void q.refetch()} />
      </>
    );
  }
  const r = q.data;
  return (
    <>
      <Link to="/releases" className="mb-4 inline-flex items-center gap-1 text-sm text-muted hover:text-accent">
        <ArrowLeft className="h-4 w-4" aria-hidden />
        {t("releases.back")}
      </Link>
      <PageHeader
        title={t("releases.detail", { id: r.id })}
        lead={`${t("releases.progress", progress(r))} · ${t("releases.col.onFailure")}: ${t(`releases.onFailure.${r.on_failure}` as MessageKey)} · ${r.requested_by ?? "—"} · ${formatDateTime(r.created_at, locale)}`}
        actions={
          <>
            <StatusBadge state={r.state} />
            <CancelRelease r={r} />
          </>
        }
      />
      {r.error && (
        <div role="alert" className="mb-4 rounded-lg border border-bad/40 bg-bad/10 p-3 text-sm">
          <p className="font-medium text-bad">{t("releases.error")}</p>
          <p className="mt-1 whitespace-pre-wrap break-words">{r.error}</p>
        </div>
      )}
      <Section title={t("releases.deploys")}>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[48rem] text-left text-sm">
            <caption className="sr-only">{t("releases.deploysCaption")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className={th}>{t("releases.col.deploy")}</th>
                <th scope="col" className={th}>{t("releases.col.target")}</th>
                <th scope="col" className={th}>{t("releases.col.version")}</th>
                <th scope="col" className={th}>{t("releases.col.after")}</th>
                <th scope="col" className={th}>{t("releases.col.state")}</th>
                <th scope="col" className={th}>{t("releases.col.rollback")}</th>
              </tr>
            </thead>
            <tbody>
              {r.deploys.map((d) => (
                <tr key={d.name} className="border-b border-border/40 align-top last:border-0">
                  <th scope="row" className={`${td} font-normal`}>
                    <Link to={`/deployments/${encodeURIComponent(d.deployment_id)}`} className="font-mono font-medium text-fg hover:text-accent hover:underline">
                      {d.name}
                    </Link>
                  </th>
                  <td className={td}>
                    {d.application} <span aria-hidden>→</span>
                    <span className="sr-only"> / </span> {d.environment}
                  </td>
                  <td className={`${td} font-mono`}>{d.version || "—"}</td>
                  <td className={`${td} font-mono text-xs`}>{d.after?.length ? d.after.join(", ") : "—"}</td>
                  <td className={td}>
                    <StatusBadge state={d.state} />
                    {d.error && <p className="mt-1 max-w-sm break-words text-xs text-bad">{d.error}</p>}
                  </td>
                  <td className={td}>
                    {d.rollback_id ? (
                      <span className="flex flex-wrap items-center gap-2">
                        <Link to={`/deployments/${encodeURIComponent(d.rollback_id)}`} className="text-xs hover:text-accent hover:underline">
                          <Badge tone="warn">{t("kind.rollback")}</Badge>
                        </Link>
                        {d.rollback_state && <StatusBadge state={d.rollback_state} />}
                      </span>
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>
    </>
  );
}
