import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, Hourglass, PauseCircle, RefreshCw, XCircle, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { Link } from "react-router";

import { unwrap, type Activity, type Status, type Worker } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { Badge, Button, EmptyState, ErrorState, Loading, PageHeader, Section, StatusBadge } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";
import { formatDateTime, formatRelative, shortId } from "@/lib/format";
import { canSomewhere } from "@/lib/permissions";

// Activity changes all the time: refreshed more often than the other pages.
const REFRESH = 5_000;

const th = "px-4 py-3 font-medium";
const td = "px-4 py-3";

function DeploymentCell({ d }: { d: Status }) {
  const { t } = useI18n();
  return (
    <th scope="row" className={`${td} font-normal`}>
      <Link to={`/deployments/${encodeURIComponent(d.id)}`} className="font-medium text-fg underline-offset-4 hover:text-accent hover:underline">
        {d.application ?? "—"} <span aria-hidden>→</span>
        <span className="sr-only"> / </span> {d.environment ?? "—"}
      </Link>
      <div className="flex flex-wrap items-center gap-1 font-mono text-xs text-muted">
        {shortId(d.id)}
        {d.dry_run && <Badge tone="violet">{t("deployments.dryRun")}</Badge>}
      </div>
    </th>
  );
}

function When({ iso }: { iso?: string }) {
  const { locale } = useI18n();
  if (!iso) return <>—</>;
  return (
    <time dateTime={iso} title={formatDateTime(iso, locale)}>
      {formatRelative(iso, locale)}
    </time>
  );
}

/** Group is a table of the active deploys in one state; it is not shown if empty. */
function Group({ title, items, columns, row }: { title: string; items: Status[]; columns: string[]; row: (d: Status) => ReactNode }) {
  if (items.length === 0) return null;
  return (
    <Section title={`${title} (${items.length})`}>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[44rem] text-left text-sm">
          <caption className="sr-only">{title}</caption>
          <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
            <tr>
              {columns.map((c) => (
                <th key={c} scope="col" className={th}>
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {items.map((d) => (
              <tr key={d.id} className="border-b border-border/40 last:border-0 hover:bg-raised/40">
                {row(d)}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Section>
  );
}

function ActiveDeployments({ a }: { a: Activity }) {
  const { t } = useI18n();
  const empty = a.running.length + a.awaiting_approval.length + a.waiting_dependency.length + a.queued.length + a.delayed.length === 0;
  if (empty) return <EmptyState>{t("activity.empty")}</EmptyState>;
  const c = (k: MessageKey) => t(k);
  return (
    <div className="flex flex-col gap-4">
      <Group
        title={t("activity.running")}
        items={a.running}
        columns={[c("activity.col.deployment"), c("activity.col.version"), c("activity.col.worker"), c("activity.col.progress"), c("activity.col.since")]}
        row={(d) => (
          <>
            <DeploymentCell d={d} />
            <td className={`${td} font-mono`}>{d.version || "—"}</td>
            <td className={`${td} font-mono text-xs`}>{d.executor || "—"}</td>
            <td className={td}>
              <span className="flex flex-wrap gap-1">
                {d.last_step && <Badge tone="info">{t("activity.step", { step: d.last_step })}</Badge>}
                {d.rollout_progress && <Badge tone="violet">{t("activity.waves", { progress: d.rollout_progress })}</Badge>}
                {!d.last_step && !d.rollout_progress && "—"}
              </span>
            </td>
            <td className={td}>
              <When iso={d.started_at ?? d.created_at} />
            </td>
          </>
        )}
      />
      <Group
        title={t("activity.awaiting")}
        items={a.awaiting_approval}
        columns={[c("activity.col.deployment"), c("activity.col.version"), c("activity.col.requestedBy"), c("activity.col.state"), c("activity.col.since")]}
        row={(d) => (
          <>
            <DeploymentCell d={d} />
            <td className={`${td} font-mono`}>{d.version || "—"}</td>
            <td className={td}>{d.requested_by || "—"}</td>
            <td className={td}>
              <StatusBadge state={d.state} />
            </td>
            <td className={td}>
              <When iso={d.created_at} />
            </td>
          </>
        )}
      />
      <Group
        title={t("activity.waitingDependency")}
        items={a.waiting_dependency}
        columns={[c("activity.col.deployment"), c("activity.col.version"), c("activity.col.requestedBy"), c("activity.col.waitingFor"), c("activity.col.since")]}
        row={(d) => (
          <>
            <DeploymentCell d={d} />
            <td className={`${td} font-mono`}>{d.version || "—"}</td>
            <td className={td}>{d.requested_by || "—"}</td>
            <td className={td}>
              <ul className="flex flex-col gap-1">
                {(d.after?.dependencies ?? [])
                  .filter((x) => x.state !== "SUCCESS")
                  .map((x) => (
                    <li key={x.id} className="flex flex-wrap items-center gap-2">
                      <Link to={`/deployments/${encodeURIComponent(x.id)}`} className="font-mono text-xs hover:text-accent hover:underline">
                        {shortId(x.id)}
                      </Link>
                      <StatusBadge state={x.state} />
                    </li>
                  ))}
              </ul>
            </td>
            <td className={td}>
              <When iso={d.created_at} />
            </td>
          </>
        )}
      />
      <Group
        title={t("activity.queued")}
        items={a.queued}
        columns={[c("activity.col.deployment"), c("activity.col.version"), c("activity.col.requestedBy"), c("activity.col.queue"), c("activity.col.since")]}
        row={(d) => (
          <>
            <DeploymentCell d={d} />
            <td className={`${td} font-mono`}>{d.version || "—"}</td>
            <td className={td}>{d.requested_by || "—"}</td>
            <td className={`${td} font-mono text-xs`}>{d.queue || "—"}</td>
            <td className={td}>
              <When iso={d.created_at} />
            </td>
          </>
        )}
      />
      <Group
        title={t("activity.delayed")}
        items={a.delayed}
        columns={[c("activity.col.deployment"), c("activity.col.version"), c("activity.col.requestedBy"), c("activity.col.startsAt")]}
        row={(d) => (
          <>
            <DeploymentCell d={d} />
            <td className={`${td} font-mono`}>{d.version || "—"}</td>
            <td className={td}>{d.requested_by || "—"}</td>
            <td className={td}>
              <When iso={d.scheduled_at} />
            </td>
          </>
        )}
      />
      {a.truncated && <p className="text-sm text-muted">{t("activity.truncated")}</p>}
    </div>
  );
}

const workerStyle: Record<string, { tone: "ok" | "bad" | "muted" | "warn"; icon: LucideIcon }> = {
  alive: { tone: "ok", icon: CheckCircle2 },
  stale: { tone: "bad", icon: XCircle },
  stopped: { tone: "muted", icon: PauseCircle },
  unregistered: { tone: "warn", icon: CircleDashed },
};

function WorkerState({ state }: { state: string }) {
  const { t } = useI18n();
  const s = workerStyle[state] ?? { tone: "muted" as const, icon: Hourglass };
  const key = `worker.${state}` as MessageKey;
  return (
    <Badge tone={s.tone} icon={s.icon}>
      {t(key) === key ? state : t(key)}
    </Badge>
  );
}

function Workers() {
  const { t } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["workers"],
    queryFn: async (): Promise<Worker[]> => unwrap(await api.GET("/api/v1/workers")).workers,
    refetchInterval: REFRESH,
  });
  return (
    <Section
      title={t("activity.workers")}
      actions={
        <Link to="/platform" className="text-sm hover:text-accent hover:underline">
          {t("activity.platformLink")}
        </Link>
      }
    >
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.data.length === 0 ? (
        <EmptyState>{t("activity.noWorkers")}</EmptyState>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[52rem] text-left text-sm">
            <caption className="sr-only">{t("activity.workers")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className={th}>{t("activity.col.worker")}</th>
                <th scope="col" className={th}>{t("activity.col.state")}</th>
                <th scope="col" className={th}>{t("activity.col.region")}</th>
                <th scope="col" className={th}>{t("activity.col.version")}</th>
                <th scope="col" className={th}>{t("activity.col.heartbeat")}</th>
                <th scope="col" className={th}>{t("activity.col.tasks")}</th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((w) => (
                <tr key={w.id} className="border-b border-border/40 align-top last:border-0">
                  <th scope="row" className={`${td} font-normal`}>
                    <div className="font-mono font-medium">{w.id}</div>
                    {w.hostname && w.hostname !== w.id && <div className="font-mono text-xs text-muted">{w.hostname}</div>}
                  </th>
                  <td className={td}>
                    <WorkerState state={w.state} />
                    {w.recovered_by && <div className="mt-1 text-xs text-muted">{t("activity.recoveredBy", { who: w.recovered_by })}</div>}
                  </td>
                  <td className={`${td} font-mono`}>{w.region || "—"}</td>
                  <td className={`${td} font-mono text-xs`}>
                    {w.version || "—"}
                    {w.build && <div className="text-muted">{w.build}</div>}
                  </td>
                  <td className={td}>
                    <When iso={w.heartbeat_at} />
                  </td>
                  <td className={td}>
                    {w.tasks.length === 0 && !w.hidden_tasks ? (
                      <span className="text-muted">{t("activity.idle")}</span>
                    ) : (
                      <ul className="flex flex-col gap-1">
                        {w.tasks.map((task) => (
                          <li key={task.id} className="flex flex-wrap items-center gap-2">
                            <Badge tone={task.kind === "approval" ? "warn" : "info"}>{t(`task.${task.kind}` as MessageKey)}</Badge>
                            <Link to={`/deployments/${encodeURIComponent(task.deployment_id)}`} className="hover:text-accent hover:underline">
                              {task.application ? `${task.application}/${task.environment}` : shortId(task.deployment_id)}
                            </Link>
                            {task.region && <span className="font-mono text-xs text-muted">{task.region}</span>}
                            <span className="text-xs text-muted">
                              <When iso={task.started_at} />
                            </span>
                          </li>
                        ))}
                        {w.hidden_tasks ? <li className="text-xs text-muted">{t("activity.hidden", { count: w.hidden_tasks })}</li> : null}
                      </ul>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}

export function ActivityPage() {
  const { t } = useI18n();
  const api = useApi();
  const me = useMe();
  const q = useQuery({
    queryKey: ["activity"],
    queryFn: async (): Promise<Activity> => unwrap(await api.GET("/api/v1/activity")),
    refetchInterval: REFRESH,
  });
  return (
    <>
      <PageHeader
        title={t("activity.title")}
        lead={t("activity.lead")}
        actions={
          <Button icon={RefreshCw} onClick={() => void q.refetch()} busy={q.isRefetching}>
            {t("common.refresh")}
          </Button>
        }
      />
      <div className="flex flex-col gap-6">
        {q.isPending ? <Loading /> : q.isError ? <ErrorState error={q.error} onRetry={() => void q.refetch()} /> : <ActiveDeployments a={q.data} />}
        {canSomewhere(me.data, "platform.read") && <Workers />}
      </div>
    </>
  );
}
