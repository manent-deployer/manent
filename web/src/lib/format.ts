// Formattazione di date e durate secondo la lingua dell'interfaccia.

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

/** formatRelative dice quanto tempo fa (o tra quanto) cade l'istante, es. "3 min fa". */
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

/** formatDuration rende una durata in millisecondi, es. "2 min 5 s" o "450 ms". */
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

/** durationBetween restituisce i millisecondi tra due istanti ISO (fino ad ora se manca la fine). */
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

export function shortId(id: string): string {
  return id.length > 14 ? id.slice(0, 8) + "…" : id;
}
