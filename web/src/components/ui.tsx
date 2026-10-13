import * as RDialog from "@radix-ui/react-dialog";
import clsx from "clsx";
import {
  AlertTriangle,
  Ban,
  CheckCircle2,
  CircleDashed,
  Clock,
  Copy,
  Hourglass,
  Link2,
  Loader2,
  PauseCircle,
  ShieldQuestion,
  Undo2,
  Unlink,
  X,
  XCircle,
  type LucideIcon,
} from "lucide-react";
import { forwardRef, useId, useState, type ButtonHTMLAttributes, type ReactNode } from "react";

import { ApiError } from "@/api/client";
import { useI18n } from "@/i18n/i18n";
import type { MessageKey } from "@/i18n/en";

// --- Pulsanti --------------------------------------------------------------------------------

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

const variants: Record<ButtonVariant, string> = {
  primary: "bg-accent text-accent-fg hover:bg-accent/90 shadow-glow",
  secondary: "border border-border bg-raised/60 text-fg hover:bg-raised",
  ghost: "text-muted hover:bg-raised/70 hover:text-fg",
  danger: "border border-bad/60 bg-bad/10 text-bad hover:bg-bad/20",
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: "sm" | "md";
  busy?: boolean;
  icon?: LucideIcon;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", size = "md", busy, icon: Icon, className, children, disabled, type = "button", ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      className={clsx(
        "inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        size === "sm" ? "h-8 px-3 text-sm" : "h-10 px-4 text-sm",
        variants[variant],
        className,
      )}
      {...rest}
    >
      {busy ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : Icon ? <Icon className="h-4 w-4" aria-hidden /> : null}
      {children}
    </button>
  );
});

// --- Intestazione di pagina -------------------------------------------------------------------

export function PageHeader({ title, lead, actions }: { title: string; lead?: string; actions?: ReactNode }) {
  return (
    <header className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 id="page-title" tabIndex={-1} className="gradient-text text-3xl font-bold outline-none">
          {title}
        </h1>
        {lead && <p className="mt-1 max-w-prose text-muted">{lead}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  );
}

export function Section({ title, children, id, actions }: { title: string; children: ReactNode; id?: string; actions?: ReactNode }) {
  const auto = useId();
  const hid = id ?? auto;
  return (
    <section aria-labelledby={hid} className="card p-4 sm:p-5">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h2 id={hid} className="text-lg font-semibold">
          <span className="mr-1 font-mono text-sm text-accent/80" aria-hidden>
            //
          </span>
          {title}
        </h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

// --- Stati -----------------------------------------------------------------------------------

type Tone = "ok" | "bad" | "warn" | "info" | "muted" | "violet";

const tones: Record<Tone, string> = {
  ok: "border-ok/40 bg-ok/10 text-ok",
  bad: "border-bad/40 bg-bad/10 text-bad",
  warn: "border-warn/40 bg-warn/10 text-warn",
  info: "border-accent/40 bg-accent/10 text-accent",
  muted: "border-border bg-raised/60 text-muted",
  violet: "border-violet/40 bg-violet/10 text-violet",
};

export function Badge({ tone = "muted", icon: Icon, children, spin }: { tone?: Tone; icon?: LucideIcon; children: ReactNode; spin?: boolean }) {
  return (
    <span className={clsx("inline-flex items-center gap-1.5 whitespace-nowrap rounded-full border px-2.5 py-0.5 text-xs font-medium", tones[tone])}>
      {Icon && <Icon className={clsx("h-3.5 w-3.5", spin && "animate-spin motion-reduce:animate-none")} aria-hidden />}
      {children}
    </span>
  );
}

const stateStyle: Record<string, { tone: Tone; icon: LucideIcon; spin?: boolean }> = {
  SUCCESS: { tone: "ok", icon: CheckCircle2 },
  ERROR: { tone: "bad", icon: XCircle },
  MAX_RECOVERY_ATTEMPTS_EXCEEDED: { tone: "bad", icon: XCircle },
  CANCELLED: { tone: "muted", icon: Ban },
  REJECTED: { tone: "bad", icon: Ban },
  EXPIRED: { tone: "muted", icon: Hourglass },
  PENDING: { tone: "info", icon: Loader2, spin: true },
  ENQUEUED: { tone: "muted", icon: CircleDashed },
  DELAYED: { tone: "violet", icon: Clock },
  SCHEDULED: { tone: "violet", icon: Clock },
  AWAITING_APPROVAL: { tone: "warn", icon: ShieldQuestion },
  WAITING_DEPENDENCY: { tone: "violet", icon: Link2 },
  DEPENDENCY_FAILED: { tone: "bad", icon: Unlink },
};

/** StatusBadge mostra lo stato di un deploy con icona e testo: il colore non è mai l'unico segnale. */
export function StatusBadge({ state }: { state: string }) {
  const { t } = useI18n();
  const s = stateStyle[state] ?? { tone: "muted" as Tone, icon: CircleDashed };
  const key = `state.${state}` as MessageKey;
  return (
    <Badge tone={s.tone} icon={s.icon} spin={s.spin}>
      {t(key) === key ? state : t(key)}
    </Badge>
  );
}

const stageStyle: Record<string, { tone: Tone; icon: LucideIcon; spin?: boolean }> = {
  pending: { tone: "muted", icon: CircleDashed },
  running: { tone: "info", icon: Loader2, spin: true },
  baking: { tone: "violet", icon: Hourglass },
  succeeded: { tone: "ok", icon: CheckCircle2 },
  failed: { tone: "bad", icon: XCircle },
  "rolled-back": { tone: "warn", icon: Undo2 },
  "paused-approval": { tone: "warn", icon: ShieldQuestion },
  "paused-freeze": { tone: "warn", icon: PauseCircle },
  planned: { tone: "muted", icon: CircleDashed },
};

export function StageBadge({ state, prefix = "stage" }: { state: string; prefix?: "stage" | "rollout" }) {
  const { t } = useI18n();
  const s = stageStyle[state] ?? { tone: "muted" as Tone, icon: CircleDashed };
  const key = `${prefix}.${state}` as MessageKey;
  return (
    <Badge tone={s.tone} icon={s.icon} spin={s.spin}>
      {t(key) === key ? state : t(key)}
    </Badge>
  );
}

// --- Caricamento, errori, vuoto -----------------------------------------------------------

export function Loading({ label }: { label?: string }) {
  const { t } = useI18n();
  return (
    <div role="status" className="flex items-center gap-2 py-8 text-muted">
      <Loader2 className="h-5 w-5 animate-spin motion-reduce:animate-none" aria-hidden />
      <span>{label ?? t("app.loading")}</span>
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useI18n();
  const msg = error instanceof Error ? error.message : String(error);
  const forbidden = error instanceof ApiError && error.status === 403;
  return (
    <div role="alert" className="flex flex-wrap items-start gap-3 rounded-xl border border-bad/40 bg-bad/10 p-4">
      <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-bad" aria-hidden />
      <p className="min-w-0 flex-1 break-words">{forbidden ? msg : t("common.loadError", { message: msg })}</p>
      {onRetry && (
        <Button size="sm" onClick={onRetry}>
          {t("app.retry")}
        </Button>
      )}
    </div>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return <p className="rounded-xl border border-dashed border-border p-6 text-center text-muted">{children}</p>;
}

// --- Campi ---------------------------------------------------------------------------------

interface FieldProps {
  label: string;
  hint?: string;
  error?: string;
  children: (props: { id: string; "aria-describedby"?: string; "aria-invalid"?: boolean }) => ReactNode;
}

/** Field collega etichetta, suggerimento ed errore al controllo con id e aria-describedby. */
export function Field({ label, hint, error, children }: FieldProps) {
  const id = useId();
  const hintId = hint ? `${id}-hint` : undefined;
  const errId = error ? `${id}-err` : undefined;
  const described = [hintId, errId].filter(Boolean).join(" ") || undefined;
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children({ id, "aria-describedby": described, "aria-invalid": error ? true : undefined })}
      {hint && (
        <p id={hintId} className="text-xs text-muted">
          {hint}
        </p>
      )}
      {error && (
        <p id={errId} className="text-xs text-bad">
          {error}
        </p>
      )}
    </div>
  );
}

export const inputClass =
  "h-10 w-full rounded-lg border border-border bg-bg/60 px-3 text-sm text-fg placeholder:text-muted/80 aria-[invalid=true]:border-bad";

// --- Copia --------------------------------------------------------------------------------

export function CopyButton({ value, label }: { value: string; label: string }) {
  const { t } = useI18n();
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className="inline-flex items-center gap-1 rounded p-1 text-muted hover:text-fg"
      aria-label={`${t("common.copy")}: ${label}`}
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        });
      }}
    >
      <Copy className="h-3.5 w-3.5" aria-hidden />
      <span className="sr-only" role="status">
        {done ? t("common.copied") : ""}
      </span>
    </button>
  );
}

// --- Dialoghi -------------------------------------------------------------------------------

/** Dialog modale accessibile (Radix): focus intrappolato, Esc per chiudere, titolo annunciato. */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  title: string;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
}) {
  const { t } = useI18n();
  return (
    <RDialog.Root open={open} onOpenChange={onOpenChange}>
      <RDialog.Portal>
        <RDialog.Overlay className="fixed inset-0 z-40 bg-black/60 backdrop-blur-sm" />
        <RDialog.Content className="card fixed left-1/2 top-1/2 z-50 max-h-[90vh] w-[min(36rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto bg-surface p-5 shadow-glow">
          <div className="mb-3 flex items-start justify-between gap-4">
            <RDialog.Title className="font-display text-xl font-semibold">{title}</RDialog.Title>
            <RDialog.Close className="rounded p-1 text-muted hover:text-fg" aria-label={t("common.close")}>
              <X className="h-5 w-5" aria-hidden />
            </RDialog.Close>
          </div>
          {description ? (
            <RDialog.Description className="mb-4 text-muted">{description}</RDialog.Description>
          ) : (
            <RDialog.Description className="sr-only">{title}</RDialog.Description>
          )}
          {children}
          {footer && <div className="mt-5 flex flex-wrap justify-end gap-2">{footer}</div>}
        </RDialog.Content>
      </RDialog.Portal>
    </RDialog.Root>
  );
}
