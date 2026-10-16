import { CheckCircle2, XCircle, X } from "lucide-react";
import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react";

import { useI18n } from "@/i18n/i18n";

interface Toast {
  id: number;
  kind: "ok" | "error";
  text: string;
}

const ToastContext = createContext<((kind: Toast["kind"], text: string) => void) | null>(null);

// Short notifications after an action. Screen readers announce them (role=status for
// successes, role=alert for errors) and errors stay until the user closes them.
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const next = useRef(1);
  const { t } = useI18n();
  const dismiss = useCallback((id: number) => setToasts((ts) => ts.filter((x) => x.id !== id)), []);
  const push = useCallback(
    (kind: Toast["kind"], text: string) => {
      const id = next.current++;
      setToasts((ts) => [...ts.slice(-3), { id, kind, text }]);
      if (kind === "ok") setTimeout(() => dismiss(id), 6000);
    },
    [dismiss],
  );
  const value = useMemo(() => push, [push]);
  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-50 flex w-[min(28rem,calc(100vw-2rem))] flex-col gap-2">
        {toasts.map((x) => (
          <div
            key={x.id}
            role={x.kind === "error" ? "alert" : "status"}
            className={`card pointer-events-auto flex items-start gap-3 p-3 shadow-glow ${x.kind === "error" ? "border-bad/60" : "border-ok/50"}`}
          >
            {x.kind === "error" ? (
              <XCircle className="mt-0.5 h-5 w-5 shrink-0 text-bad" aria-hidden />
            ) : (
              <CheckCircle2 className="mt-0.5 h-5 w-5 shrink-0 text-ok" aria-hidden />
            )}
            <p className="flex-1 text-sm">{x.text}</p>
            <button type="button" onClick={() => dismiss(x.id)} className="rounded p-1 text-muted hover:text-fg" aria-label={t("common.close")}>
              <X className="h-4 w-4" aria-hidden />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast() {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast fuori da ToastProvider");
  return ctx;
}
