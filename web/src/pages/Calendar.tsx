import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pause, Play, Trash2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";

import { unwrap, type ScheduleInfo } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { useToast } from "@/components/Toaster";
import { Badge, Button, Dialog, EmptyState, ErrorState, Loading, PageHeader, Section } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime, formatRelative } from "@/lib/format";
import { can } from "@/lib/permissions";

function Freezes() {
  const { t, locale } = useI18n();
  const api = useApi();
  const q = useQuery({ queryKey: ["freezes"], queryFn: async () => unwrap(await api.GET("/api/v1/freezes")).freezes, refetchInterval: 60_000 });
  if (q.isPending) return <Loading />;
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (q.data.length === 0) return <EmptyState>{t("calendar.freezes.empty")}</EmptyState>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[40rem] text-left text-sm">
        <caption className="sr-only">{t("calendar.freezes.caption")}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.name")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.scope")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.when")}</th>
            <th scope="col" className="py-2 font-medium">{t("calendar.col.state")}</th>
          </tr>
        </thead>
        <tbody>
          {q.data.map((w) => (
            <tr key={w.name} className="border-b border-border/40 align-top last:border-0">
              <th scope="row" className="py-2 pr-4 font-normal">
                <span className="font-medium">{w.name}</span>
                {w.reason && <p className="text-xs text-muted">{w.reason}</p>}
              </th>
              <td className="py-2 pr-4 font-mono text-xs">
                {w.applications.join(", ")} / {w.environments.join(", ")}
                {w.regions && w.regions.length > 0 && <p className="text-muted">{t("calendar.regions", { regions: w.regions.join(", ") })}</p>}
              </td>
              <td className="py-2 pr-4 text-muted">
                <time dateTime={w.start}>{formatDateTime(w.start, locale)}</time> – <time dateTime={w.end}>{formatDateTime(w.end, locale)}</time>
                {w.recurring && <span> ({t("calendar.recurring")})</span>}
              </td>
              <td className="py-2">
                {w.active ? <Badge tone="warn">{t("calendar.active")}</Badge> : <Badge tone="muted">{t("calendar.upcoming")}</Badge>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ScheduleActions({ s }: { s: ScheduleInfo }) {
  const { t } = useI18n();
  const api = useApi();
  const qc = useQueryClient();
  const toast = useToast();
  const [confirm, setConfirm] = useState(false);
  const op = useMutation({
    mutationFn: async (kind: "pause" | "resume" | "delete") => {
      const params = { params: { path: { name: s.name } } };
      if (kind === "delete") unwrap(await api.DELETE("/api/v1/schedules/{name}", params));
      else if (kind === "pause") unwrap(await api.POST("/api/v1/schedules/{name}/pause", params));
      else unwrap(await api.POST("/api/v1/schedules/{name}/resume", params));
      return kind;
    },
    onSuccess: (kind) => {
      toast("ok", t(`actions.done.${kind}`));
      setConfirm(false);
      void qc.invalidateQueries({ queryKey: ["schedules"] });
    },
    onError: (e) => toast("error", t("actions.failed", { message: e instanceof Error ? e.message : String(e) })),
  });
  const paused = s.status.toUpperCase().includes("PAUSE");
  return (
    <div className="flex flex-wrap gap-1">
      {paused ? (
        <Button size="sm" icon={Play} busy={op.isPending && op.variables === "resume"} onClick={() => op.mutate("resume")} aria-label={`${t("actions.resume")} ${s.name}`}>
          {t("actions.resume")}
        </Button>
      ) : (
        <Button size="sm" icon={Pause} busy={op.isPending && op.variables === "pause"} onClick={() => op.mutate("pause")} aria-label={`${t("actions.pause")} ${s.name}`}>
          {t("actions.pause")}
        </Button>
      )}
      <Button size="sm" variant="danger" icon={Trash2} onClick={() => setConfirm(true)} aria-label={`${t("actions.delete")} ${s.name}`}>
        {t("actions.delete")}
      </Button>
      <Dialog
        open={confirm}
        onOpenChange={setConfirm}
        title={t("calendar.deleteTitle", { name: s.name })}
        description={t("calendar.deleteBody")}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirm(false)}>
              {t("common.cancel")}
            </Button>
            <Button variant="danger" busy={op.isPending} onClick={() => op.mutate("delete")}>
              {t("actions.delete")}
            </Button>
          </>
        }
      />
    </div>
  );
}

function Schedules() {
  const { t, locale } = useI18n();
  const api = useApi();
  const me = useMe();
  const q = useQuery({ queryKey: ["schedules"], queryFn: async () => unwrap(await api.GET("/api/v1/schedules")).schedules, refetchInterval: 60_000 });
  if (q.isPending) return <Loading />;
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (q.data.length === 0) return <EmptyState>{t("calendar.schedules.empty")}</EmptyState>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[44rem] text-left text-sm">
        <caption className="sr-only">{t("calendar.schedules.caption")}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.name")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.target")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.cron")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.state")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.lastFired")}</th>
            <th scope="col" className="py-2 font-medium">{t("calendar.col.actions")}</th>
          </tr>
        </thead>
        <tbody>
          {q.data.map((s) => (
            <tr key={s.name} className="border-b border-border/40 align-top last:border-0">
              <th scope="row" className="py-2 pr-4 font-normal">
                <Link to={`/deployments?schedule=${encodeURIComponent(s.name)}`} className="font-medium hover:text-accent hover:underline">
                  {s.name}
                </Link>
              </th>
              <td className="py-2 pr-4">
                {s.application} / {s.environment}
              </td>
              <td className="py-2 pr-4 font-mono text-xs">
                {s.cron}
                {s.timezone && <span className="text-muted"> ({s.timezone})</span>}
              </td>
              <td className="py-2 pr-4">
                <Badge tone={s.status.toUpperCase().includes("PAUSE") ? "warn" : "ok"}>{s.status}</Badge>
              </td>
              <td className="py-2 pr-4 text-muted">
                {s.last_fired_at ? (
                  <time dateTime={s.last_fired_at} title={formatDateTime(s.last_fired_at, locale)}>
                    {formatRelative(s.last_fired_at, locale)}
                  </time>
                ) : (
                  "—"
                )}
              </td>
              <td className="py-2">{can(me.data, "schedule.write", s.application, s.environment) ? <ScheduleActions s={s} /> : "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function CalendarPage() {
  const { t } = useI18n();
  return (
    <>
      <PageHeader title={t("calendar.title")} lead={t("calendar.lead")} />
      <div className="flex flex-col gap-4">
        <Section title={t("calendar.freezes")}>
          <Freezes />
        </Section>
        <Section title={t("calendar.schedules")}>
          <Schedules />
        </Section>
      </div>
    </>
  );
}
