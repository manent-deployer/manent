import { Search, X } from "lucide-react";
import { useCallback, useId, type ReactNode } from "react";
import { useSearchParams } from "react-router";

import { inputClass } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";

/** useParam keeps a filter value in the query string, so a filtered view can be shared and
 * survives a reload. The empty string removes the parameter. */
export function useParam(key: string): [string, (v: string) => void] {
  const [params, setParams] = useSearchParams();
  const value = params.get(key) ?? "";
  const set = useCallback(
    (v: string) =>
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (v) next.set(key, v);
          else next.delete(key);
          return next;
        },
        { replace: true },
      ),
    [key, setParams],
  );
  return [value, set];
}

/** useClearParams returns a function that removes the given filters in one update: separate
 * setters called in a row would each start from the same query string and undo each other. */
export function useClearParams(...keys: string[]) {
  const [, setParams] = useSearchParams();
  const list = keys.join("\n");
  return useCallback(
    () =>
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          for (const k of list.split("\n")) next.delete(k);
          return next;
        },
        { replace: true },
      ),
    [list, setParams],
  );
}

/** matches says whether any of the fields contains every word of the query (case-insensitive). */
export function matches(query: string, ...fields: (string | undefined | null)[]) {
  const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return true;
  const text = fields.filter(Boolean).join(" ").toLowerCase();
  return words.every((w) => text.includes(w));
}

/** FilterBar lays out the filters of a list with the count of what is shown and a reset. */
export function FilterBar({
  children,
  shown,
  total,
  onReset,
  active,
}: {
  children: ReactNode;
  shown?: number;
  total?: number;
  onReset?: () => void;
  active?: boolean;
}) {
  const { t } = useI18n();
  return (
    <div role="search" className="mb-4 flex flex-wrap items-end gap-3">
      {children}
      <div className="flex h-10 items-center gap-3 text-sm text-muted">
        {shown !== undefined && total !== undefined && (
          <span aria-live="polite">{shown === total ? (total === 1 ? t("filters.countOne") : t("filters.count", { total })) : t("filters.shown", { shown, total })}</span>
        )}
        {active && onReset && (
          <button type="button" onClick={onReset} className="flex items-center gap-1 rounded-lg px-2 py-1 hover:bg-raised/70 hover:text-fg">
            <X className="h-3.5 w-3.5" aria-hidden />
            {t("common.reset")}
          </button>
        )}
      </div>
    </div>
  );
}

function Label({ htmlFor, children }: { htmlFor: string; children: ReactNode }) {
  return (
    <label htmlFor={htmlFor} className="text-xs font-medium uppercase tracking-wider text-muted">
      {children}
    </label>
  );
}

/** SearchFilter is a free-text filter. */
export function SearchFilter({ label, value, onChange, placeholder }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string }) {
  const id = useId();
  return (
    <div className="flex w-full flex-col gap-1 sm:w-64">
      <Label htmlFor={id}>{label}</Label>
      <div className="relative">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted" aria-hidden />
        <input id={id} type="search" className={`${inputClass} pl-9`} value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />
      </div>
    </div>
  );
}

/** SelectFilter chooses one value among options; the empty value means all. */
export function SelectFilter({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
}) {
  const { t } = useI18n();
  const id = useId();
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <select id={id} className={`${inputClass} w-auto min-w-[9rem]`} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{t("common.all")}</option>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  );
}

/** ToggleFilter is an on/off filter (e.g. "only busy"). */
export function ToggleFilter({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className="flex h-10 cursor-pointer items-center gap-2 rounded-lg border border-border bg-bg/60 px-3 text-sm">
      <input type="checkbox" className="h-4 w-4 accent-[rgb(var(--accent))]" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      {label}
    </label>
  );
}

/** distinct returns the sorted distinct non-empty values, for the options of a SelectFilter. */
export function distinct(values: (string | undefined | null)[]) {
  return [...new Set(values.filter((v): v is string => Boolean(v)))].sort();
}
