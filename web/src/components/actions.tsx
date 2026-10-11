import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleSlash, Rocket, ShieldCheck, ShieldX, Undo2, XCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import { TERMINAL, unwrap, type Status } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { useI18n } from "@/i18n/i18n";
import { can } from "@/lib/permissions";

import { useToast } from "./Toaster";
import { Button, Dialog, Field, inputClass } from "./ui";

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Invalida le query che un'azione può aver cambiato. */
function useRefresh() {
  const qc = useQueryClient();
  return () => {
    for (const k of ["deployments", "deployment", "approvals", "inventory", "schedules", "steps"]) {
      void qc.invalidateQueries({ queryKey: [k] });
    }
  };
}

// --- Approvazione ------------------------------------------------------------------------------

export function DecisionButtons({ dep }: { dep: Status }) {
  const { t } = useI18n();
  const me = useMe();
  const [open, setOpen] = useState<"approve" | "reject" | null>(null);
  if (dep.state !== "AWAITING_APPROVAL" || !can(me.data, "deployment.approve", dep.application, dep.environment)) return null;
  const self = me.data?.auth_enabled && dep.requested_by === me.data.subject && !dep.approval?.allow_self && !dep.rollout?.approval?.allow_self;
  return (
    <>
      <span className="inline-flex flex-wrap gap-2">
        <Button
          variant="primary"
          icon={ShieldCheck}
          onClick={() => setOpen("approve")}
          disabled={self}
          aria-describedby={self ? `self-${dep.id}` : undefined}
        >
          {t("actions.approve")}
        </Button>
        <Button variant="danger" icon={ShieldX} onClick={() => setOpen("reject")}>
          {t("actions.reject")}
        </Button>
      </span>
      {self && (
        <p id={`self-${dep.id}`} className="basis-full text-xs text-muted">
          {t("actions.selfApproval")}
        </p>
      )}
      {open && <DecisionDialog dep={dep} approve={open === "approve"} onClose={() => setOpen(null)} />}
    </>
  );
}

function DecisionDialog({ dep, approve, onClose }: { dep: Status; approve: boolean; onClose: () => void }) {
  const { t } = useI18n();
  const api = useApi();
  const toast = useToast();
  const refresh = useRefresh();
  const [comment, setComment] = useState("");
  const [touched, setTouched] = useState(false);
  const needComment = !approve && comment.trim() === "";
  const m = useMutation({
    mutationFn: async () => {
      const path = approve ? "/api/v1/deployments/{id}/approve" : "/api/v1/deployments/{id}/reject";
      unwrap(await api.POST(path, { params: { path: { id: dep.id } }, body: { comment: comment.trim() || undefined } }));
    },
    onSuccess: () => {
      toast("ok", t(approve ? "actions.done.approve" : "actions.done.reject"));
      refresh();
      onClose();
    },
    onError: (e) => toast("error", t("actions.failed", { message: errMessage(e) })),
  });
  const vars = { application: dep.application ?? "", environment: dep.environment ?? "", version: dep.version ?? "" };
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={t(approve ? "actions.approveTitle" : "actions.rejectTitle", vars)}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button
            variant={approve ? "primary" : "danger"}
            busy={m.isPending}
            onClick={() => {
              setTouched(true);
              if (!needComment) m.mutate();
            }}
          >
            {t(approve ? "actions.approve" : "actions.reject")}
          </Button>
        </>
      }
    >
      <Field label={t("actions.comment")} hint={t("actions.commentHint")} error={touched && needComment ? t("actions.commentRequired") : undefined}>
        {(p) => (
          <textarea
            {...p}
            className={`${inputClass} h-24 py-2`}
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            required={!approve}
            autoFocus
          />
        )}
      </Field>
    </Dialog>
  );
}

// --- Annullamento ----------------------------------------------------------------------------

export function CancelButton({ dep }: { dep: Status }) {
  const { t } = useI18n();
  const me = useMe();
  const api = useApi();
  const toast = useToast();
  const refresh = useRefresh();
  const [open, setOpen] = useState(false);
  const m = useMutation({
    mutationFn: async () => unwrap(await api.POST("/api/v1/deployments/{id}/cancel", { params: { path: { id: dep.id } } })),
    onSuccess: () => {
      toast("ok", t("actions.done.cancel"));
      refresh();
      setOpen(false);
    },
    onError: (e) => toast("error", t("actions.failed", { message: errMessage(e) })),
  });
  if (TERMINAL.has(dep.state) || !can(me.data, "deployment.cancel", dep.application, dep.environment)) return null;
  return (
    <>
      <Button variant="danger" icon={CircleSlash} onClick={() => setOpen(true)}>
        {t("actions.cancel")}
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t("actions.cancelTitle")}
        description={t("actions.cancelBody")}
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button variant="danger" busy={m.isPending} onClick={() => m.mutate()}>
              {t("actions.cancel")}
            </Button>
          </>
        }
      />
    </>
  );
}

// --- Promozione e rollback, con dry-run ------------------------------------------------------

type DryRun = { state: "idle" } | { state: "running"; id: string } | { state: "ok" } | { state: "failed"; message: string };

/**
 * useDryRun esegue la richiesta in dry-run (un vero workflow su un worker, che non modifica i
 * target) e ne attende l'esito: la conferma diventa possibile solo dopo un dry-run riuscito.
 */
function useDryRun() {
  const api = useApi();
  const [dry, setDry] = useState<DryRun>({ state: "idle" });
  const alive = useRef(true);
  useEffect(() => () => void (alive.current = false), []);
  const run = async (submit: () => Promise<{ id?: string }>) => {
    try {
      const sub = await submit();
      if (!sub.id) throw new Error("risposta senza id");
      setDry({ state: "running", id: sub.id });
      for (let i = 0; i < 300 && alive.current; i++) {
        const st = unwrap(await api.GET("/api/v1/deployments/{id}", { params: { path: { id: sub.id } } }));
        if (TERMINAL.has(st.state)) {
          setDry(st.state === "SUCCESS" ? { state: "ok" } : { state: "failed", message: st.error || st.state });
          return;
        }
        await new Promise((r) => setTimeout(r, 1000));
      }
    } catch (e) {
      setDry({ state: "failed", message: errMessage(e) });
    }
  };
  return { dry, run, reset: () => setDry({ state: "idle" }) };
}

function DryRunStatus({ dry }: { dry: DryRun }) {
  const { t } = useI18n();
  if (dry.state === "idle") return <p className="text-sm text-muted">{t("actions.dryRunFirst")}</p>;
  return (
    <p role="status" className="flex items-center gap-2 text-sm">
      {dry.state === "running" && <span className="text-muted">{t("actions.dryRunRunning")}</span>}
      {dry.state === "ok" && (
        <>
          <CheckCircle2 className="h-4 w-4 text-ok" aria-hidden />
          <span className="text-ok">{t("actions.dryRunOk")}</span>
        </>
      )}
      {dry.state === "failed" && (
        <>
          <XCircle className="h-4 w-4 shrink-0 text-bad" aria-hidden />
          <span className="text-bad">{t("actions.dryRunFailed", { message: dry.message })}</span>
        </>
      )}
    </p>
  );
}

export function PromoteDialog({
  application,
  from,
  environments,
  onClose,
}: {
  application: string;
  from: string;
  environments: string[];
  onClose: () => void;
}) {
  const { t } = useI18n();
  const api = useApi();
  const me = useMe();
  const toast = useToast();
  const refresh = useRefresh();
  const navigate = useNavigate();
  // Ambienti suggeriti (quelli dell'inventario), ma si può scriverne uno nuovo: il primo rilascio
  // in un ambiente non ha ancora una riga nell'inventario.
  const suggestions = environments.filter((e) => e !== from);
  const [to, setTo] = useState(suggestions.find((e) => can(me.data, "deployment.submit", application, e)) ?? "");
  const target = to.trim();
  const allowed = target !== "" && target !== from && can(me.data, "deployment.submit", application, target);
  const [override, setOverride] = useState("");
  const { dry, run, reset } = useDryRun();
  const canOverride = can(me.data, "deployment.freeze-override", application, to);
  const body = (dryRun: boolean) => ({ application, from, to: target, dry_run: dryRun || undefined, freeze_override: override.trim() || undefined });
  const m = useMutation({
    mutationFn: async () => unwrap(await api.POST("/api/v1/promotions", { body: body(false) })),
    onSuccess: (sub) => {
      toast("ok", t("actions.done.promote"));
      refresh();
      onClose();
      if (sub.id) navigate(`/deployments/${encodeURIComponent(sub.id)}`);
    },
    onError: (e) => toast("error", t("actions.failed", { message: errMessage(e) })),
  });
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={t("actions.promoteTitle", { application, from })}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button
            busy={dry.state === "running"}
            disabled={!allowed}
            onClick={() => void run(async () => unwrap(await api.POST("/api/v1/promotions", { body: body(true) })))}
          >
            {t("actions.dryRun")}
          </Button>
          <Button variant="primary" icon={Rocket} disabled={dry.state !== "ok"} busy={m.isPending} onClick={() => m.mutate()}>
            {t("actions.submit", { action: t("actions.promote").toLowerCase() })}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <Field
          label={t("actions.promoteTo")}
          error={target !== "" && !allowed ? t("actions.promoteNotAllowed", { environment: target }) : undefined}
        >
          {(p) => (
            <>
              <input
                {...p}
                list="promote-environments"
                className={inputClass}
                value={to}
                autoComplete="off"
                onChange={(e) => {
                  setTo(e.target.value);
                  reset();
                }}
              />
              <datalist id="promote-environments">
                {suggestions.map((e) => (
                  <option key={e} value={e} />
                ))}
              </datalist>
            </>
          )}
        </Field>
        {canOverride && (
          <Field label={t("actions.freezeOverride")} hint={t("actions.freezeOverrideHint")}>
            {(p) => (
              <input
                {...p}
                className={inputClass}
                value={override}
                onChange={(e) => {
                  setOverride(e.target.value);
                  reset();
                }}
              />
            )}
          </Field>
        )}
        <DryRunStatus dry={dry} />
      </div>
    </Dialog>
  );
}

export function RollbackDialog({ application, environment, onClose }: { application: string; environment: string; onClose: () => void }) {
  const { t } = useI18n();
  const api = useApi();
  const me = useMe();
  const toast = useToast();
  const refresh = useRefresh();
  const navigate = useNavigate();
  const [version, setVersion] = useState("");
  const [override, setOverride] = useState("");
  const { dry, run, reset } = useDryRun();
  const canOverride = can(me.data, "deployment.freeze-override", application, environment);
  const body = (dryRun: boolean) => ({
    application,
    environment,
    version: version.trim() || undefined,
    dry_run: dryRun || undefined,
    freeze_override: override.trim() || undefined,
  });
  const m = useMutation({
    mutationFn: async () => unwrap(await api.POST("/api/v1/rollbacks", { body: body(false) })),
    onSuccess: (sub) => {
      toast("ok", t("actions.done.rollback"));
      refresh();
      onClose();
      if (sub.id) navigate(`/deployments/${encodeURIComponent(sub.id)}`);
    },
    onError: (e) => toast("error", t("actions.failed", { message: errMessage(e) })),
  });
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={t("actions.rollbackTitle", { application, environment })}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button busy={dry.state === "running"} onClick={() => void run(async () => unwrap(await api.POST("/api/v1/rollbacks", { body: body(true) })))}>
            {t("actions.dryRun")}
          </Button>
          <Button variant="primary" icon={Undo2} disabled={dry.state !== "ok"} busy={m.isPending} onClick={() => m.mutate()}>
            {t("actions.submit", { action: t("actions.rollback").toLowerCase() })}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <Field label={t("actions.rollbackVersion")} hint={t("actions.rollbackVersionHint")}>
          {(p) => (
            <input
              {...p}
              className={inputClass}
              value={version}
              onChange={(e) => {
                setVersion(e.target.value);
                reset();
              }}
            />
          )}
        </Field>
        {canOverride && (
          <Field label={t("actions.freezeOverride")} hint={t("actions.freezeOverrideHint")}>
            {(p) => (
              <input
                {...p}
                className={inputClass}
                value={override}
                onChange={(e) => {
                  setOverride(e.target.value);
                  reset();
                }}
              />
            )}
          </Field>
        )}
        <DryRunStatus dry={dry} />
      </div>
    </Dialog>
  );
}
