import clsx from "clsx";
import { useEffect, useMemo, useRef, useState } from "react";

import type { LogLine } from "@/api/client";
import { useI18n } from "@/i18n/i18n";

import { EmptyState, Field, inputClass } from "./ui";

const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];
const levelRank = (l: string) => Math.max(0, LEVELS.indexOf(l.toUpperCase()));
const levelClass: Record<string, string> = { DEBUG: "text-muted", INFO: "text-accent", WARN: "text-warn", ERROR: "text-bad" };

/**
 * LogViewer mostra le righe di log come testo (mai HTML: possono contenere output dei comandi
 * remoti), filtrabili per livello e step. Con "segui" scorre alle righe nuove.
 */
export function LogViewer({ lines }: { lines: LogLine[] }) {
  const { t, locale } = useI18n();
  const [level, setLevel] = useState("INFO");
  const [step, setStep] = useState("");
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);
  const steps = useMemo(() => [...new Set(lines.map((l) => l.step).filter(Boolean))] as string[], [lines]);
  const shown = useMemo(
    () => lines.filter((l) => levelRank(l.level) >= levelRank(level) && (!step || l.step === step)),
    [lines, level, step],
  );
  const time = useMemo(() => new Intl.DateTimeFormat(locale, { timeStyle: "medium" }), [locale]);

  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [shown, follow]);

  return (
    <div>
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <Field label={t("detail.logs.level")}>
          {(p) => (
            <select {...p} className={clsx(inputClass, "w-36")} value={level} onChange={(e) => setLevel(e.target.value)}>
              {LEVELS.map((l) => (
                <option key={l}>{l}</option>
              ))}
            </select>
          )}
        </Field>
        <Field label={t("detail.logs.step")}>
          {(p) => (
            <select {...p} className={clsx(inputClass, "w-56")} value={step} onChange={(e) => setStep(e.target.value)}>
              <option value="">{t("common.all")}</option>
              {steps.map((s) => (
                <option key={s}>{s}</option>
              ))}
            </select>
          )}
        </Field>
        <label className="flex h-10 items-center gap-2 text-sm">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} className="h-4 w-4 accent-[rgb(var(--accent))]" />
          {t("detail.logs.follow")}
        </label>
        <span className="ml-auto self-center text-xs text-muted">{t("detail.logs.count", { count: shown.length })}</span>
      </div>
      {shown.length === 0 ? (
        <EmptyState>{t("detail.logs.empty")}</EmptyState>
      ) : (
        <div
          ref={box}
          role="log"
          aria-label={t("detail.logs.region")}
          aria-live="off"
          tabIndex={0}
          className="max-h-[28rem] overflow-auto rounded-lg border border-border/70 bg-bg/70 p-3 font-mono text-xs leading-relaxed"
        >
          {shown.map((l, i) => (
            <div key={i} className="grid grid-cols-[auto_auto_1fr] gap-x-3 whitespace-pre-wrap break-words">
              <time dateTime={l.time} className="text-muted">
                {time.format(new Date(l.time))}
              </time>
              <span className={clsx("w-11", levelClass[l.level.toUpperCase()] ?? "text-muted")}>{l.level.toUpperCase()}</span>
              <span>
                {l.step && <span className="text-violet">[{l.step}] </span>}
                {l.message}
                {l.attrs &&
                  Object.entries(l.attrs).map(([k, v]) => (
                    <span key={k} className="text-muted">
                      {" "}
                      {k}={v}
                    </span>
                  ))}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
