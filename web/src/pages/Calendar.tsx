import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pause, Play, Plus, ShieldAlert, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";

import { unwrap, type ScheduleInfo, type WindowStatus } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { useToast } from "@/components/Toaster";
import { Badge, Button, Dialog, EmptyState, ErrorState, Field, inputClass, Loading, PageHeader, Section } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime, formatRelative } from "@/lib/format";
import { can, canSomewhere } from "@/lib/permissions";

/** list splits a comma-separated list of patterns. */
function list(v: string): string[] {
  return v
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);
}

/** NewFreeze creates a freeze window at runtime: an emergency freeze without touching manent.yml. */
function NewFreeze() {
  const { t } = useI18n();
  const api = useApi();
  const qc = useQueryClient();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [reason, setReason] = useState("");
  const [apps, setApps] = useState("*");
  const [envs, setEnvs] = useState("prod");
  const [hours, setHours] = useState("4");
  const [hard, setHard] = useState(false);
  const create = useMutation({
    mutationFn: async () => {
      const h = Number(hours);
      const to = new Date(Date.now() + h * 3_600_000).toISOString();
      return unwrap(await api.POST("/api/v1/freezes", { body: { name, reason, applications: list(apps), environments: list(envs), to, hard } }));
    },
    onSuccess: (w) => {
      toast("ok", t("calendar.freeze.created", { name: w.name }));
      setOpen(false);
      void qc.invalidateQueries({ queryKey: ["freezes"] });
    },
    onError: (e) => toast("error", t("actions.failed", { message: e instanceof Error ? e.message : String(e) })),
  });
  const invalidHours = !(Number(hours) > 0);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!invalidHours && name) create.mutate();
  };
  return (
    <>
      <Button size="sm" icon={Plus} onClick={() => setOpen(true)}>
        {t("calendar.freeze.new")}
      </Button>
      <Dialog open={open} onOpenChange={setOpen} title={t("calendar.freeze.new")} description={t("calendar.freeze.lead")}>
        <form className="flex flex-col gap-3" onSubmit={submit}>
          <Field label={t("calendar.freeze.name")} hint={t("calendar.freeze.nameHint")}>
            {(p) => <input {...p} className={inputClass} value={name} onChange={(e) => setName(e.target.value)} required pattern="[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}" />}
          </Field>
          <Field label={t("calendar.freeze.reason")}>{(p) => <input {...p} className={inputClass} value={reason} onChange={(e) => setReason(e.target.value)} />}</Field>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label={t("calendar.freeze.applications")} hint={t("calendar.freeze.patternsHint")}>
              {(p) => <input {...p} className={inputClass} value={apps} onChange={(e) => setApps(e.target.value)} required />}
            </Field>
            <Field label={t("calendar.freeze.environments")} hint={t("calendar.freeze.patternsHint")}>
              {(p) => <input {...p} className={inputClass} value={envs} onChange={(e) => setEnvs(e.target.value)} required />}
            </Field>
          </div>
          <Field label={t("calendar.freeze.hours")} error={invalidHours ? t("calendar.freeze.hoursInvalid") : undefined}>
            {(p) => <input {...p} type="number" min="0.25" step="0.25" className={inputClass} value={hours} onChange={(e) => setHours(e.target.value)} />}
          </Field>
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-1" checked={hard} onChange={(e) => setHard(e.target.checked)} />
            <span>
              {t("calendar.freeze.hard")}
              <span className="block text-xs text-muted">{t("calendar.freeze.hardHint")}</span>
            </span>
          </label>
          <div className="mt-2 flex flex-wrap justify-end gap-2">
            <Button variant="ghost" type="button" onClick={() => setOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button variant="primary" type="submit" busy={create.isPending} disabled={!name || invalidHours}>
              {t("calendar.freeze.create")}
            </Button>
          </div>
        </form>
      </Dialog>
    </>
  );
}

/** DeleteFreeze closes early a window created at runtime. */
function DeleteFreeze({ w }: { w: WindowStatus }) {
  const { t } = useI18n();
  const api = useApi();
  const qc = useQueryClient();
  const toast = useToast();
  const [confirm, setConfirm] = useState(false);
  const del = useMutation({
    mutationFn: async () => unwrap(await api.DELETE("/api/v1/freezes/{name}", { params: { path: { name: w.name } } })),
    onSuccess: () => {
      toast("ok", t("calendar.freeze.deleted", { name: w.name }));
      setConfirm(false);
      void qc.invalidateQueries({ queryKey: ["freezes"] });
    },
    onError: (e) => toast("error", t("actions.failed", { message: e instanceof Error ? e.message : String(e) })),
  });
  return (
    <>
      <Button size="sm" variant="danger" icon={Trash2} onClick={() => setConfirm(true)} aria-label={`${t("calendar.freeze.delete")} ${w.name}`}>
        {t("calendar.freeze.delete")}
      </Button>
      <Dialog
        open={confirm}
        onOpenChange={setConfirm}
        title={t("calendar.freeze.deleteTitle", { name: w.name })}
        description={t("calendar.freeze.deleteBody")}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirm(false)}>
              {t("common.cancel")}
            </Button>
            <Button variant="danger" busy={del.isPending} onClick={() => del.mutate()}>
              {t("calendar.freeze.delete")}
            </Button>
          </>
        }
      />
    </>
  );
}

/** canWriteFreeze says whether the identity has freeze.write on every pattern of the window. */
function canWriteFreeze(me: Parameters<typeof can>[0], w: WindowStatus): boolean {
  return w.applications.every((a) => w.environments.every((e) => can(me, "freeze.write", a, e)));
}

function Freezes() {
  const { t, locale } = useI18n();
  const api = useApi();
  const me = useMe();
  const q = useQuery({ queryKey: ["freezes"], queryFn: async () => unwrap(await api.GET("/api/v1/freezes")).freezes, refetchInterval: 60_000 });
  if (q.isPending) return <Loading />;
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (q.data.length === 0) return <EmptyState>{t("calendar.freezes.empty")}</EmptyState>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[48rem] text-left text-sm">
        <caption className="sr-only">{t("calendar.freezes.caption")}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.name")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.scope")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.when")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("calendar.col.state")}</th>
            <th scope="col" className="py-2 font-medium">{t("calendar.col.actions")}</th>
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
              <td className="py-2 pr-4">
                <div className="flex flex-wrap gap-1">
                  {w.active ? <Badge tone="warn">{t("calendar.active")}</Badge> : <Badge tone="muted">{t("calendar.upcoming")}</Badge>}
                  {w.hard && (
                    <Badge tone="bad" icon={ShieldAlert}>
                      {t("calendar.hard")}
                    </Badge>
                  )}
                  {w.dynamic && <Badge tone="info">{t("calendar.dynamic", { who: w.created_by ?? "" })}</Badge>}
                </div>
              </td>
              <td className="py-2">{w.dynamic && canWriteFreeze(me.data, w) ? <DeleteFreeze w={w} /> : "—"}</td>
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
  const me = useMe();
  return (
    <>
      <PageHeader title={t("calendar.title")} lead={t("calendar.lead")} />
      <div className="flex flex-col gap-4">
        <Section title={t("calendar.freezes")} actions={canSomewhere(me.data, "freeze.write") ? <NewFreeze /> : undefined}>
          <Freezes />
        </Section>
        <Section title={t("calendar.schedules")}>
          <Schedules />
        </Section>
      </div>
    </>
  );
}
