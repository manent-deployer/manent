import { ArrowLeft, Radio, RefreshCw } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { ApiError, type Status, type StepRecord } from "@/api/client";
import { CancelButton, DecisionButtons } from "@/components/actions";
import { LogViewer } from "@/components/LogViewer";
import { RolloutView } from "@/components/RolloutView";
import { Badge, CopyButton, EmptyState, ErrorState, Loading, PageHeader, Section, StatusBadge } from "@/components/ui";
import { useDeployment } from "@/hooks/useDeployment";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";
import { formatDateTime, formatDuration, formatRelative } from "@/lib/format";

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wider text-muted">{label}</dt>
      <dd className="mt-0.5 break-words">{children}</dd>
    </div>
  );
}

function Facts({ d }: { d: Status }) {
  const { t, locale } = useI18n();
  return (
    <dl className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <Fact label={t("detail.id")}>
        <span className="font-mono text-sm">{d.id}</span>
        <CopyButton value={d.id} label={t("detail.id")} />
      </Fact>
      <Fact label={t("detail.artifact")}>{d.artifact || "—"}</Fact>
      <Fact label={t("detail.version")}>
        <span className="font-mono">{d.version || "—"}</span>
      </Fact>
      <Fact label={t("detail.kind")}>
        <span className="flex flex-wrap items-center gap-2">
          {t(`kind.${d.kind ?? "deploy"}` as MessageKey)}
          {d.dry_run && <Badge tone="violet">{t("deployments.dryRun")}</Badge>}
        </span>
      </Fact>
      {d.sha256 && (
        <Fact label={t("detail.sha256")}>
          <span className="font-mono text-xs" title={d.sha256}>
            {d.sha256.slice(0, 16)}…
          </span>
          <CopyButton value={d.sha256} label="sha256" />
        </Fact>
      )}
      {d.source && (
        <Fact label={t("detail.source")}>
          <Link className="font-mono text-sm text-accent hover:underline" to={`/deployments/${encodeURIComponent(d.source)}`}>
            {d.source}
          </Link>
        </Fact>
      )}
      {d.release && (
        <Fact label={t("detail.release")}>
          <Link className="font-mono text-sm text-accent hover:underline" to={`/releases/${encodeURIComponent(d.release)}`}>
            {d.release}
          </Link>
        </Fact>
      )}
      <Fact label={t("detail.requestedBy")}>{d.requested_by || "—"}</Fact>
      <Fact label={t("detail.created")}>
        <time dateTime={d.created_at}>{formatDateTime(d.created_at, locale)}</time>
      </Fact>
      <Fact label={t("detail.updated")}>
        <time dateTime={d.updated_at}>{formatDateTime(d.updated_at, locale)}</time>
      </Fact>
      {d.scheduled_at && (
        <Fact label={t("detail.scheduledAt")}>
          <time dateTime={d.scheduled_at}>{formatDateTime(d.scheduled_at, locale)}</time>
        </Fact>
      )}
      {d.rollback && <Fact label={t("detail.rollback")}>{d.rollback}</Fact>}
    </dl>
  );
}

/** DependenciesPanel mostra i deploy da cui questo dipende (after) con il loro stato. */
function DependenciesPanel({ d }: { d: Status }) {
  const { t, locale } = useI18n();
  const a = d.after;
  if (!a) return null;
  return (
    <Section title={t("detail.after")}>
      <p className="text-sm text-muted">
        {a.status === "satisfied" ? t("detail.after.satisfied") : t("detail.after.lead")}
        {a.status === "waiting" && (
          <>
            {" "}
            <time dateTime={a.expires_at} title={formatDateTime(a.expires_at, locale)}>
              {t("detail.after.expires", { time: formatRelative(a.expires_at, locale) })}
            </time>
          </>
        )}
      </p>
      <ul className="mt-3 flex flex-col gap-2" aria-label={t("detail.after.caption")}>
        {a.dependencies.map((dep) => (
          <li key={dep.id} className="flex flex-wrap items-center gap-3">
            <Link className="font-mono text-sm text-accent hover:underline" to={`/deployments/${encodeURIComponent(dep.id)}`}>
              {dep.id}
            </Link>
            <StatusBadge state={dep.state} />
          </li>
        ))}
      </ul>
      {a.reason && <p className="mt-3 text-sm text-bad">{a.reason}</p>}
    </Section>
  );
}

function ApprovalPanel({ d }: { d: Status }) {
  const { t, locale } = useI18n();
  const a = d.rollout?.approval ?? d.approval;
  if (!a) return null;
  const approvals = a.decisions?.filter((x) => x.approve).length ?? 0;
  return (
    <Section title={t("detail.approval")} actions={<DecisionButtons dep={d} />}>
      <p className="text-sm">
        {t("detail.approval.required", { done: approvals, required: a.required })}
        {a.status === "pending" && (
          <span className="text-muted"> · {t("detail.approval.expires", { time: formatRelative(a.expires_at, locale) })}</span>
        )}
      </p>
      {a.decisions && a.decisions.length > 0 && (
        <ul className="mt-3 space-y-2">
          {a.decisions.map((x, i) => (
            <li key={i} className="rounded-lg border border-border/60 p-3 text-sm">
              <p className={x.approve ? "text-ok" : "text-bad"}>
                {t(x.approve ? "detail.approval.decision.approve" : "detail.approval.decision.reject", { who: x.approver })}{" "}
                <time className="text-muted" dateTime={x.time}>
                  {formatRelative(x.time, locale)}
                </time>
              </p>
              {x.comment && <p className="mt-1 whitespace-pre-wrap text-muted">{x.comment}</p>}
            </li>
          ))}
        </ul>
      )}
    </Section>
  );
}

function StepsTable({ steps }: { steps: StepRecord[] }) {
  const { t, locale } = useI18n();
  if (steps.length === 0) return <EmptyState>{t("detail.steps.empty")}</EmptyState>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[36rem] text-left text-sm">
        <caption className="sr-only">{t("detail.steps.caption")}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className="py-2 pr-4 font-medium">{t("detail.steps.col.step")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("detail.steps.col.started")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("detail.steps.col.duration")}</th>
            <th scope="col" className="py-2 font-medium">{t("detail.steps.col.outcome")}</th>
          </tr>
        </thead>
        <tbody>
          {steps.map((s) => (
            <tr key={s.id} className="border-b border-border/40 align-top last:border-0">
              <th scope="row" className="py-2 pr-4 font-mono text-xs font-normal">
                {s.name}
              </th>
              <td className="py-2 pr-4 text-muted">
                <time dateTime={s.started_at}>{formatDateTime(s.started_at, locale)}</time>
              </td>
              <td className="py-2 pr-4 font-mono text-xs">{formatDuration(s.duration_ms)}</td>
              <td className="py-2">
                {s.error ? (
                  <>
                    <Badge tone="bad">{t("detail.steps.failed")}</Badge>
                    <p className="mt-1 max-w-xl whitespace-pre-wrap break-words text-xs text-bad">{s.error}</p>
                  </>
                ) : s.completed_at ? (
                  <Badge tone="ok">{t("detail.steps.ok")}</Badge>
                ) : (
                  <Badge tone="info">{t("detail.steps.running")}</Badge>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function DeploymentDetailPage() {
  const { id = "" } = useParams();
  const { t } = useI18n();
  const { status, steps, lines, mode } = useDeployment(id);
  const d = status.data;

  // Annuncia agli screen reader i cambi di stato del deploy (non il primo caricamento).
  const [announce, setAnnounce] = useState("");
  const prev = useRef<string>();
  useEffect(() => {
    if (!d) return;
    if (prev.current && prev.current !== d.state) setAnnounce(t("detail.stateChanged", { state: t(`state.${d.state}` as MessageKey) }));
    prev.current = d.state;
  }, [d, t]);

  if (status.isPending) return <Loading />;
  if (status.isError) {
    const notFound = status.error instanceof ApiError && status.error.status === 404;
    return (
      <>
        <PageHeader title={notFound ? t("common.notFound") : t("app.error.title")} />
        <ErrorState error={status.error} onRetry={notFound ? undefined : () => void status.refetch()} />
      </>
    );
  }
  if (!d) return null;

  return (
    <>
      <Link to="/deployments" className="mb-4 inline-flex items-center gap-1 text-sm text-muted hover:text-accent">
        <ArrowLeft className="h-4 w-4" aria-hidden />
        {t("detail.back")}
      </Link>
      <PageHeader
        title={t("detail.title", { application: d.application ?? "—", environment: d.environment ?? "—" })}
        actions={
          <>
            <StatusBadge state={d.state} />
            <Badge tone={mode === "stream" ? "info" : "muted"} icon={mode === "stream" ? Radio : RefreshCw}>
              {mode === "stream" ? t("detail.live") : t("detail.liveOff")}
            </Badge>
            <CancelButton dep={d} />
          </>
        }
      />
      <p className="sr-only" role="status" aria-live="polite">
        {announce}
      </p>
      <div className="flex flex-col gap-4">
        <Section title={t("detail.facts")}>
          <Facts d={d} />
          {d.error && (
            <div role="alert" className="mt-4 rounded-lg border border-bad/40 bg-bad/10 p-3 text-sm">
              <p className="font-medium text-bad">{t("detail.error")}</p>
              <p className="mt-1 whitespace-pre-wrap break-words">{d.error}</p>
            </div>
          )}
        </Section>
        <DependenciesPanel d={d} />
        <ApprovalPanel d={d} />
        {d.rollout && (
          <Section title={t("detail.rollout")}>
            <RolloutView rollout={d.rollout} />
          </Section>
        )}
        <Section title={t("detail.steps")}>
          {steps.isPending ? <Loading /> : steps.isError ? <ErrorState error={steps.error} /> : <StepsTable steps={steps.data ?? []} />}
        </Section>
        <Section title={t("detail.logs")}>
          <LogViewer lines={lines} />
        </Section>
      </div>
    </>
  );
}
