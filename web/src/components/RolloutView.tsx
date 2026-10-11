import clsx from "clsx";
import { ChevronRight, Server } from "lucide-react";

import type { RolloutState } from "@/api/client";
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

/**
 * RolloutView disegna il piano a ondate: gli stadi in ordine di esecuzione (lista ordinata,
 * leggibile anche da uno screen reader) e, dentro ogni stadio, i passi per region e AZ.
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
