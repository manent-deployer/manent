// Formatting of dates and durations in the interface language.

export function formatDateTime(iso: string | undefined, locale: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "—";
  return new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "medium" }).format(d);
}

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 31536000],
  ["month", 2592000],
  ["week", 604800],
  ["day", 86400],
  ["hour", 3600],
  ["minute", 60],
  ["second", 1],
];

/** formatRelative says how long ago (or how far ahead) the instant is, e.g. "3 min ago". */
export function formatRelative(iso: string | undefined, locale: string, now = Date.now()): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const diff = (t - now) / 1000;
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto", style: "short" });
  for (const [unit, secs] of UNITS) {
    if (Math.abs(diff) >= secs || unit === "second") {
      return rtf.format(Math.round(diff / secs), unit);
    }
  }
  return "—";
}

/** formatDuration renders a duration in milliseconds, e.g. "2 min 5 s" or "450 ms". */
export function formatDuration(ms: number | undefined): string {
  if (ms === undefined || ms < 0 || Number.isNaN(ms)) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `${h} h ${m} min`;
  if (m > 0) return sec > 0 ? `${m} min ${sec} s` : `${m} min`;
  return `${sec} s`;
}

/** durationBetween returns the milliseconds between two ISO instants (up to now if the end is missing). */
export function durationBetween(start?: string, end?: string, now = Date.now()): number | undefined {
  if (!start) return undefined;
  const a = new Date(start).getTime();
  const b = end ? new Date(end).getTime() : now;
  if (Number.isNaN(a) || Number.isNaN(b)) return undefined;
  return Math.max(0, b - a);
}

export function formatPercent(v: number | undefined, locale: string): string {
  if (v === undefined || Number.isNaN(v)) return "—";
  return new Intl.NumberFormat(locale, { style: "percent", maximumFractionDigits: 1 }).format(v);
}

export function formatNumber(v: number | undefined, locale: string, digits = 2): string {
  if (v === undefined || Number.isNaN(v)) return "—";
  return new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(v);
}

/** formatBytes formats a size with binary units (KiB, MiB, GiB…); "—" if unknown. */
export function formatBytes(n: number | undefined, locale: string): string {
  if (!n) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: i === 0 ? 0 : 1 }).format(v)} ${units[i]}`;
}

export function shortId(id: string): string {
  return id.length > 14 ? id.slice(0, 8) + "…" : id;
}

/** tenantLabel hides "untenanted", the reserved tenant of the applications no tenant lists:
 * without tenants it would be on every row. */
export function tenantLabel(tenant: string | undefined): string | undefined {
  return tenant && tenant !== "untenanted" ? tenant : undefined;
}
