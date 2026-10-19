import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, CircleDashed, Cpu, HardDrive, Hourglass, MemoryStick, PauseCircle, RefreshCw, XCircle, type LucideIcon } from "lucide-react";
import clsx from "clsx";
import { Link } from "react-router-dom";

import { unwrap, type Queue, type Worker, type WorkerResources } from "@/api/client";
import { useApi } from "@/api/context";
import { Badge, Button, EmptyState, ErrorState, Loading, PageHeader, Section } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";
import { formatBytes, formatDateTime, formatNumber, formatRelative, shortId } from "@/lib/format";

// Workers and queues change all the time, as the activity does.
const REFRESH = 5_000;

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

function When({ iso }: { iso?: string }) {
  const { locale } = useI18n();
  if (!iso) return <>—</>;
  return (
    <time dateTime={iso} title={formatDateTime(iso, locale)}>
      {formatRelative(iso, locale)}
    </time>
  );
}

/** Meter shows a used/available value as a bar, with the numbers as text for everyone. */
function Meter({ icon: Icon, label, used, total, text }: { icon: LucideIcon; label: string; used?: number; total?: number; text: string }) {
  const ratio = used !== undefined && total ? Math.min(used / total, 1) : undefined;
  const tone = ratio === undefined ? "bg-border" : ratio >= 0.9 ? "bg-bad" : ratio >= 0.7 ? "bg-warn" : "bg-ok";
  return (
    <div className="min-w-0">
      <div className="flex items-center justify-between gap-2 text-xs">
        <span className="flex items-center gap-1.5 uppercase tracking-wider text-muted">
          <Icon className="h-3.5 w-3.5" aria-hidden />
          {label}
        </span>
        <span className="font-mono">{text}</span>
      </div>
      <div
        className="mt-1 h-1.5 overflow-hidden rounded-full bg-raised"
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={total ?? 0}
        aria-valuenow={used ?? 0}
        aria-valuetext={text}
      >
        {ratio !== undefined && <div className={clsx("h-full rounded-full", tone)} style={{ width: `${Math.max(ratio * 100, 2)}%` }} />}
      </div>
    </div>
  );
}

/** memoryCapacity is the memory the worker may use: the container limit, otherwise the host memory. */
function memoryCapacity(r: WorkerResources) {
  return r.memory_limit_bytes || r.memory_total_bytes;
}

function Resources({ w }: { w: Worker }) {
  const { t, locale } = useI18n();
  const r = w.resources;
  if (!r) return <p className="text-sm text-muted">{t("platform.noResources")}</p>;
  const busy = w.tasks.filter((task) => task.kind === "deploy" || task.kind === "pass").length;
  const capacity = memoryCapacity(r);
  return (
    <div className="flex flex-col gap-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <Meter
          icon={Cpu}
          label={t("platform.cpu")}
          used={r.process_cpu_cores}
          total={r.cpu_limit}
          text={t("platform.cpuText", { used: formatNumber(r.process_cpu_cores, locale), limit: r.cpu_limit })}
        />
        <Meter
          icon={MemoryStick}
          label={t("platform.memory")}
          used={r.process_memory_bytes}
          total={capacity}
          text={`${formatBytes(r.process_memory_bytes, locale)} / ${formatBytes(capacity, locale)}`}
        />
        {r.concurrency ? (
          <Meter
            icon={Hourglass}
            label={t("platform.slots")}
            used={busy}
            total={r.concurrency}
            text={t("platform.slotsText", { busy, total: r.concurrency })}
          />
        ) : null}
        {r.workdir_free_bytes ? (
          <Meter icon={HardDrive} label={t("platform.disk")} text={t("platform.diskText", { free: formatBytes(r.workdir_free_bytes, locale) })} />
        ) : null}
      </div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-muted sm:grid-cols-4">
        <div>
          <dt className="inline">{t("platform.host")}: </dt>
          <dd className="inline font-mono text-fg">
            {r.os}/{r.arch}, {t("platform.hostCpus", { n: r.cpus })}
          </dd>
        </div>
        {r.memory_total_bytes ? (
          <div>
            <dt className="inline">{t("platform.hostMemory")}: </dt>
            <dd className="inline font-mono text-fg">
              {r.memory_available_bytes
                ? t("platform.hostMemoryText", { available: formatBytes(r.memory_available_bytes, locale), total: formatBytes(r.memory_total_bytes, locale) })
                : formatBytes(r.memory_total_bytes, locale)}
            </dd>
          </div>
        ) : null}
        {r.load1 !== undefined ? (
          <div>
            <dt className="inline">{t("platform.load")}: </dt>
            <dd className="inline font-mono text-fg">
              {formatNumber(r.load1, locale)} · {formatNumber(r.load5, locale)} · {formatNumber(r.load15, locale)}
            </dd>
          </div>
        ) : null}
        <div>
          <dt className="inline">{t("platform.sampled")}: </dt>
          <dd className="inline text-fg">
            <When iso={r.sampled_at} />
          </dd>
        </div>
      </dl>
    </div>
  );
}

function WorkerCard({ w }: { w: Worker }) {
  const { t } = useI18n();
  return (
    <li className="card flex flex-col gap-4 p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="font-mono text-base font-semibold">{w.id}</h3>
          <p className="font-mono text-xs text-muted">
            {[w.hostname !== w.id ? w.hostname : undefined, w.region && t("platform.region", { region: w.region }), w.build].filter(Boolean).join(" · ") || "—"}
          </p>
        </div>
        <div className="flex flex-col items-end gap-1 text-xs text-muted">
          <WorkerState state={w.state} />
          <span>
            {t("platform.heartbeat")} <When iso={w.heartbeat_at} />
          </span>
        </div>
      </div>
      <Resources w={w} />
      <div>
        <h4 className="text-xs uppercase tracking-wider text-muted">{t("platform.queues")}</h4>
        {w.queues && w.queues.length > 0 ? (
          <ul className="mt-1 flex flex-wrap gap-1">
            {w.queues.map((q) => (
              <li key={q}>
                <Badge tone="info">{q}</Badge>
              </li>
            ))}
          </ul>
        ) : (
          <p className="mt-1 text-sm text-muted">—</p>
        )}
      </div>
      <div>
        <h4 className="text-xs uppercase tracking-wider text-muted">{t("activity.col.tasks")}</h4>
        {w.tasks.length === 0 && !w.hidden_tasks ? (
          <p className="mt-1 text-sm text-muted">{t("activity.idle")}</p>
        ) : (
          <ul className="mt-1 flex flex-col gap-1 text-sm">
            {w.tasks.map((task) => (
              <li key={task.id} className="flex flex-wrap items-center gap-2">
                <Badge tone={task.kind === "approval" ? "warn" : "info"}>{t(`task.${task.kind}` as MessageKey)}</Badge>
                <Link to={`/deployments/${encodeURIComponent(task.deployment_id)}`} className="hover:text-accent hover:underline">
                  {task.application ? `${task.application}/${task.environment}` : shortId(task.deployment_id)}
                </Link>
                {task.queue && <span className="font-mono text-xs text-muted">{task.queue}</span>}
                <span className="text-xs text-muted">
                  <When iso={task.started_at} />
                </span>
              </li>
            ))}
            {w.hidden_tasks ? <li className="text-xs text-muted">{t("activity.hidden", { count: w.hidden_tasks })}</li> : null}
          </ul>
        )}
      </div>
    </li>
  );
}

function limits(q: Queue, t: ReturnType<typeof useI18n>["t"]) {
  const parts: string[] = [];
  if (q.worker_concurrency) parts.push(t("platform.limit.worker", { n: q.worker_concurrency }));
  if (q.partition_concurrency) parts.push(t("platform.limit.partition", { n: q.partition_concurrency }));
  if (q.global_concurrency) parts.push(t("platform.limit.global", { n: q.global_concurrency }));
  return parts.length > 0 ? parts.join(", ") : t("platform.limit.none");
}

const th = "px-4 py-3 font-medium";
const td = "px-4 py-3";

function Queues({ queues }: { queues: Queue[] }) {
  const { t } = useI18n();
  if (queues.length === 0) return <EmptyState>{t("platform.noQueues")}</EmptyState>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[56rem] text-left text-sm">
        <caption className="sr-only">{t("platform.queuesTitle")}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className={th}>{t("platform.col.queue")}</th>
            <th scope="col" className={th}>{t("platform.col.enqueued")}</th>
            <th scope="col" className={th}>{t("platform.col.running")}</th>
            <th scope="col" className={th}>{t("platform.col.delayed")}</th>
            <th scope="col" className={th}>{t("platform.col.oldest")}</th>
            <th scope="col" className={th}>{t("platform.col.limits")}</th>
            <th scope="col" className={th}>{t("platform.col.workers")}</th>
          </tr>
        </thead>
        <tbody>
          {queues.map((q) => (
            <tr key={q.name} className="border-b border-border/40 align-top last:border-0">
              <th scope="row" className={`${td} font-normal`}>
                <div className="font-mono font-medium">{q.name}</div>
                <div className="text-xs text-muted">
                  {t(`platform.kind.${q.kind}` as MessageKey)}
                  {q.region && ` · ${q.region}`}
                </div>
              </th>
              <td className={`${td} font-mono`}>
                {q.enqueued > 0 ? <Badge tone={q.workers.length === 0 ? "bad" : "warn"}>{q.enqueued}</Badge> : 0}
              </td>
              <td className={`${td} font-mono`}>{q.running}</td>
              <td className={`${td} font-mono`}>{q.delayed}</td>
              <td className={td}>
                <When iso={q.oldest_enqueued} />
              </td>
              <td className={`${td} text-xs`}>{limits(q, t)}</td>
              <td className={td}>
                {q.workers.length === 0 ? (
                  <Badge tone={q.enqueued + q.running > 0 ? "bad" : "muted"}>{t("platform.noListeners")}</Badge>
                ) : (
                  <ul className="flex flex-wrap gap-1">
                    {q.workers.map((w) => (
                      <li key={w} className="font-mono text-xs">
                        {w}
                      </li>
                    ))}
                  </ul>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function PlatformPage() {
  const { t } = useI18n();
  const api = useApi();
  const workers = useQuery({
    queryKey: ["workers"],
    queryFn: async (): Promise<Worker[]> => unwrap(await api.GET("/api/v1/workers")).workers,
    refetchInterval: REFRESH,
  });
  const queues = useQuery({
    queryKey: ["queues"],
    queryFn: async (): Promise<Queue[]> => unwrap(await api.GET("/api/v1/queues")).queues,
    refetchInterval: REFRESH,
  });
  return (
    <>
      <PageHeader
        title={t("platform.title")}
        lead={t("platform.lead")}
        actions={
          <Button
            icon={RefreshCw}
            onClick={() => {
              void workers.refetch();
              void queues.refetch();
            }}
            busy={workers.isRefetching || queues.isRefetching}
          >
            {t("common.refresh")}
          </Button>
        }
      />
      <div className="flex flex-col gap-6">
        <Section title={t("platform.queuesTitle")}>
          {queues.isPending ? (
            <Loading />
          ) : queues.isError ? (
            <ErrorState error={queues.error} onRetry={() => void queues.refetch()} />
          ) : (
            <Queues queues={queues.data} />
          )}
        </Section>
        <Section title={t("activity.workers")}>
          {workers.isPending ? (
            <Loading />
          ) : workers.isError ? (
            <ErrorState error={workers.error} onRetry={() => void workers.refetch()} />
          ) : workers.data.length === 0 ? (
            <EmptyState>{t("activity.noWorkers")}</EmptyState>
          ) : (
            <ul className="grid gap-4 lg:grid-cols-2">
              {workers.data.map((w) => (
                <WorkerCard key={w.id} w={w} />
              ))}
            </ul>
          )}
        </Section>
      </div>
    </>
  );
}
