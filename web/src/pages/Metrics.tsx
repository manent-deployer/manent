import { useQuery } from "@tanstack/react-query";
import { Activity, AlertOctagon, Clock, GitPullRequestArrow } from "lucide-react";
import { useSearchParams } from "react-router";
import { Bar, BarChart, CartesianGrid, Legend, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";

import { unwrap, type DORAMetrics } from "@/api/client";
import { useApi } from "@/api/context";
import { EmptyState, ErrorState, Field, Loading, PageHeader, Section, inputClass } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatDuration, formatNumber, formatPercent } from "@/lib/format";

const PERIODS = [7, 30, 90];

function Kpi({ label, value, detail, icon: Icon }: { label: string; value: string; detail?: string; icon: typeof Activity }) {
  return (
    <div className="card flex flex-col gap-1 p-4">
      <dt className="flex items-center gap-2 text-sm text-muted">
        <Icon className="h-4 w-4 text-accent" aria-hidden />
        {label}
      </dt>
      <dd className="font-display text-3xl font-semibold">{value}</dd>
      {detail && <dd className="text-xs text-muted">{detail}</dd>}
    </div>
  );
}

function label(m: DORAMetrics) {
  return `${m.application ?? "—"} / ${m.environment ?? "—"}`;
}

export function MetricsPage() {
  const { t, locale } = useI18n();
  const api = useApi();
  const [params, setParams] = useSearchParams();
  const days = Number(params.get("days")) || 30;
  const q = useQuery({
    queryKey: ["dora", days],
    queryFn: async () => unwrap(await api.GET("/api/v1/metrics/dora", { params: { query: { since: `${days}d` } } })),
  });
  const total = q.data?.total;
  const rows = (q.data?.environments ?? []).filter((m) => m.deployments > 0);
  const chart = rows.map((m) => ({ name: label(m), ok: m.deployments - m.failures, failed: m.failures }));

  return (
    <>
      <PageHeader
        title={t("metrics.title")}
        lead={t("metrics.lead")}
        actions={
          <Field label={t("metrics.period")}>
            {(p) => (
              <select
                {...p}
                className={`${inputClass} w-48`}
                value={days}
                onChange={(e) => setParams(new URLSearchParams({ days: e.target.value }))}
              >
                {PERIODS.map((d) => (
                  <option key={d} value={d}>
                    {t("metrics.days", { days: d })}
                  </option>
                ))}
              </select>
            )}
          </Field>
        }
      />
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !total || total.deployments === 0 ? (
        <EmptyState>{t("metrics.empty")}</EmptyState>
      ) : (
        <div className="flex flex-col gap-4">
          <dl className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Kpi
              icon={Activity}
              label={t("metrics.frequency")}
              value={formatNumber(total.deployments_per_day, locale)}
              detail={t("metrics.deployments", { count: total.deployments, failures: total.failures })}
            />
            <Kpi icon={AlertOctagon} label={t("metrics.failureRate")} value={formatPercent(total.change_failure_rate, locale)} />
            <Kpi icon={Clock} label={t("metrics.mttr")} value={formatDuration(total.mttr_seconds ? total.mttr_seconds * 1000 : undefined)} />
            <Kpi
              icon={GitPullRequestArrow}
              label={t("metrics.leadTime")}
              value={formatDuration(total.lead_time_seconds ? total.lead_time_seconds * 1000 : undefined)}
            />
          </dl>
          <Section title={t("metrics.byEnvironment")}>
            <figure>
              <div className="h-72 w-full" aria-hidden>
                <ResponsiveContainer>
                  <BarChart data={chart} margin={{ top: 8, right: 8, bottom: 8, left: 0 }}>
                    <CartesianGrid stroke="rgb(var(--border))" strokeOpacity={0.5} vertical={false} />
                    <XAxis dataKey="name" tick={{ fill: "rgb(var(--muted))", fontSize: 12 }} stroke="rgb(var(--border))" />
                    <YAxis allowDecimals={false} tick={{ fill: "rgb(var(--muted))", fontSize: 12 }} stroke="rgb(var(--border))" />
                    <Tooltip
                      cursor={{ fill: "rgb(var(--raised))", fillOpacity: 0.5 }}
                      contentStyle={{ background: "rgb(var(--surface))", border: "1px solid rgb(var(--border))", borderRadius: 8, color: "rgb(var(--fg))" }}
                    />
                    <Legend wrapperStyle={{ color: "rgb(var(--muted))" }} />
                    <Bar dataKey="ok" name={t("metrics.succeeded")} stackId="a" fill="rgb(var(--accent))" radius={[0, 0, 0, 0]} />
                    <Bar dataKey="failed" name={t("metrics.failed")} stackId="a" fill="rgb(var(--bad))" radius={[4, 4, 0, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
              <figcaption className="mt-2 text-sm text-muted">{t("metrics.chart")}</figcaption>
            </figure>
            <details className="mt-3">
              <summary className="cursor-pointer text-sm text-accent">{t("common.showTable")}</summary>
              <div className="mt-2 overflow-x-auto">
                <table className="w-full min-w-[40rem] text-left text-sm">
                  <caption className="sr-only">{t("metrics.caption")}</caption>
                  <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
                    <tr>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("metrics.col.target")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("metrics.frequency")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("metrics.failureRate")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("metrics.mttr")}</th>
                      <th scope="col" className="py-2 font-medium">{t("metrics.leadTime")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((m) => (
                      <tr key={label(m)} className="border-b border-border/40 last:border-0">
                        <th scope="row" className="py-2 pr-4 font-normal">
                          {label(m)}
                          <p className="text-xs text-muted">{t("metrics.deployments", { count: m.deployments, failures: m.failures })}</p>
                        </th>
                        <td className="py-2 pr-4">{formatNumber(m.deployments_per_day, locale)}</td>
                        <td className="py-2 pr-4">{formatPercent(m.change_failure_rate, locale)}</td>
                        <td className="py-2 pr-4">{formatDuration(m.mttr_seconds ? m.mttr_seconds * 1000 : undefined)}</td>
                        <td className="py-2">{formatDuration(m.lead_time_seconds ? m.lead_time_seconds * 1000 : undefined)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </details>
          </Section>
        </div>
      )}
    </>
  );
}
