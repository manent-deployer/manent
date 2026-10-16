import clsx from "clsx";
import { ChevronRight, Server } from "lucide-react";

import type { RolloutState, TrafficState } from "@/api/client";
import { useI18n } from "@/i18n/i18n";

import { StageBadge } from "./ui";

const stageBorder: Record<string, string> = {
  succeeded: "border-ok/50",
  failed: "border-bad/60",
  "rolled-back": "border-warn/60",
  running: "border-accent/70 shadow-glow",
  baking: "border-violet/60",
  pending: "border-border",
};

/** TrafficBar shows the traffic share of the candidate version against the stable one. */
function TrafficBar({ traffic }: { traffic: TrafficState }) {
  const { t } = useI18n();
  const w = Math.max(0, Math.min(100, traffic.weight));
  return (
    <div className="mb-4 space-y-1.5">
      <div
        className="flex h-3 overflow-hidden rounded-full bg-border"
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={w}
        aria-label={t("detail.rollout.traffic.weight", { weight: w })}
      >
        <div className="bg-muted/40" style={{ width: `${100 - w}%` }} />
        <div className="bg-accent" style={{ width: `${w}%` }} />
      </div>
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
        <span>{t("detail.rollout.traffic.stable", { slot: traffic.stable || "-", weight: 100 - w })}</span>
        <span className="text-accent">{t("detail.rollout.traffic.candidate", { slot: traffic.candidate || "-", weight: w })}</span>
        {traffic.router && <span>{t("detail.rollout.traffic.router", { router: traffic.router })}</span>}
        {traffic.testers && <span>{t("detail.rollout.traffic.testers")}</span>}
        {traffic.promoted && <span className="text-ok">{t("detail.rollout.traffic.promoted")}</span>}
      </div>
    </div>
  );
}

/**
 * RolloutView draws the wave plan: the stages in execution order (ordered list, readable by
 * screen readers too) and, inside every stage, the passes per region and AZ.
 */
export function RolloutView({ rollout }: { rollout: RolloutState }) {
  const { t } = useI18n();
  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-3 text-sm">
        <StageBadge state={rollout.status} prefix="rollout" />
        <span className="text-muted">{t("detail.rollout.onFailure", { policy: rollout.on_failure })}</span>
        {rollout.paused && <span className="text-warn">{t("detail.rollout.paused", { reason: rollout.paused })}</span>}
      </div>
      {rollout.traffic && <TrafficBar traffic={rollout.traffic} />}
      <ol aria-label={t("detail.rollout.stageList")} className="flex flex-wrap items-stretch gap-2">
        {rollout.stages.map((s, i) => (
          <li key={s.name} className="flex items-stretch gap-2">
            <div
              className={clsx(
                "flex w-60 flex-col gap-2 rounded-xl border bg-bg/40 p-3",
                stageBorder[s.state] ?? "border-border",
                rollout.stage === s.name && "ring-1 ring-accent/50",
              )}
              aria-current={rollout.stage === s.name ? "step" : undefined}
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <p className="font-mono text-xs text-muted">{s.wave}</p>
                  <p className="truncate font-medium" title={s.name}>
                    {s.name}
                  </p>
                </div>
                <StageBadge state={s.state} />
              </div>
              <ul className="space-y-1 text-xs">
                {s.passes.map((p) => (
                  <li key={p.name} className="flex items-center gap-1.5 text-muted">
                    <Server className="h-3.5 w-3.5 shrink-0" aria-hidden />
                    <span className="truncate font-mono" title={p.hosts.join(", ")}>
                      {[p.region, p.az].filter(Boolean).join(" / ") || p.name}
                    </span>
                    {p.drained && p.drained.length > 0 && (
                      <span className="shrink-0 text-warn">{t("detail.rollout.drained", { count: p.drained.length })}</span>
                    )}
                    <span className="ml-auto shrink-0">{t("detail.rollout.hosts", { count: p.hosts.length })}</span>
                  </li>
                ))}
              </ul>
              {s.gate && <p className="rounded-md bg-bad/10 p-2 text-xs text-bad">{t("detail.rollout.gate", { gate: s.gate })}</p>}
              {s.error && <p className="rounded-md bg-bad/10 p-2 text-xs text-bad">{s.error}</p>}
            </div>
            {i < rollout.stages.length - 1 && <ChevronRight className="hidden h-5 w-5 self-center text-muted sm:block" aria-hidden />}
          </li>
        ))}
      </ol>
    </div>
  );
}
