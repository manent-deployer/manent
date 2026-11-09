import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FileText, Lock, RefreshCw } from "lucide-react";
import clsx from "clsx";
import { useMemo } from "react";

import { unwrap, type Configuration } from "@/api/client";
import { useApi } from "@/api/context";
import { FilterBar, SearchFilter, ToggleFilter, useClearParams, useParam } from "@/components/Filters";
import { Badge, Button, CopyButton, EmptyState, ErrorState, Loading, PageHeader, Section } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";

type Value = unknown;
type Obj = Record<string, Value>;

function isObj(v: Value): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** isEmpty says whether a value carries nothing: hidden unless asked, defaults are mostly empty. */
function isEmpty(v: Value) {
  return v === null || v === undefined || v === "" || (Array.isArray(v) && v.length === 0) || (isObj(v) && Object.keys(v).length === 0);
}

function scalarText(v: Value) {
  return v === null || v === undefined ? "" : String(v);
}

/** prune keeps the values that match the query (in their path or in the value; a matching key
 * keeps all it holds) and, with hideEmpty, drops the empty ones. undefined = dropped. */
function prune(v: Value, path: string, words: string[], hideEmpty: boolean): Value | undefined {
  const hit = (text: string) => words.every((w) => text.includes(w));
  if (words.length > 0 && hit(path.toLowerCase())) {
    words = [];
  }
  if (isObj(v)) {
    const out: Obj = {};
    for (const [k, child] of Object.entries(v)) {
      const p = prune(child, path ? `${path}.${k}` : k, words, hideEmpty);
      if (p !== undefined) out[k] = p;
    }
    return Object.keys(out).length === 0 && (hideEmpty || words.length > 0) ? undefined : out;
  }
  if (Array.isArray(v)) {
    if (v.every((x) => !isObj(x) && !Array.isArray(x))) {
      if (hideEmpty && v.length === 0) return undefined;
      return words.length === 0 || hit(`${path} ${v.map(scalarText).join(" ")}`.toLowerCase()) ? v : undefined;
    }
    const items = v.map((x, i) => prune(x, `${path}[${i}]`, words, hideEmpty));
    if (items.every((x) => x === undefined)) return undefined;
    // Keep the positions: a hidden item becomes null, shown as a gap, so [i] stays right.
    return items.map((x) => (x === undefined ? null : x));
  }
  if (hideEmpty && isEmpty(v)) return undefined;
  return words.length === 0 || hit(`${path} ${scalarText(v)}`.toLowerCase()) ? v : undefined;
}

/** itemLabel names a list item by its index and, when it has one, its name (pipeline steps,
 * queues, providers, rules): "[2] qa". */
function itemLabel(item: Value, i: number) {
  const name = isObj(item) && typeof item.name === "string" && item.name ? item.name : undefined;
  return name ? `[${i}] ${name}` : `[${i}]`;
}

function Scalar({ value, redacted }: { value: Value; redacted: boolean }) {
  const { t } = useI18n();
  if (redacted) {
    return (
      <span className="inline-flex items-center gap-1 rounded bg-warn/15 px-1.5 font-mono text-xs text-warn" title={t("config.redactedHint")}>
        <Lock className="h-3 w-3" aria-hidden />
        {scalarText(value)}
      </span>
    );
  }
  if (typeof value === "boolean") return <span className={clsx("font-mono", value ? "text-ok" : "text-muted")}>{String(value)}</span>;
  if (typeof value === "number") return <span className="font-mono text-violet">{value}</span>;
  if (value === null || value === undefined || value === "") return <span className="text-muted">—</span>;
  return <span className="break-all font-mono">{String(value)}</span>;
}

function Tree({ value, path, redacted }: { value: Obj; path: string; redacted: Set<string> }) {
  return (
    <dl className="flex flex-col">
      {Object.entries(value).map(([k, v]) => (
        <Entry key={k} name={k} value={v} path={path ? `${path}.${k}` : k} redacted={redacted} />
      ))}
    </dl>
  );
}

function Entry({ name, value, path, redacted }: { name: string; value: Value; path: string; redacted: Set<string> }) {
  if (isObj(value) || (Array.isArray(value) && value.some((x) => isObj(x) || Array.isArray(x)))) {
    return (
      <div className="border-l border-border/60 pl-3">
        <details open className="group">
          <summary className="flex cursor-pointer list-none items-center gap-1 py-1 font-mono text-sm text-muted hover:text-fg">
            <ChevronRight className="h-3.5 w-3.5 transition-transform group-open:rotate-90" aria-hidden />
            <dt className="font-medium text-fg">{name}</dt>
            {Array.isArray(value) && <span className="text-xs">[{value.length}]</span>}
          </summary>
          <dd className="ml-1.5 pb-1">
            {isObj(value) ? (
              <Tree value={value} path={path} redacted={redacted} />
            ) : (
              <ol className="flex flex-col">
                {(value as Value[]).map((item, i) =>
                  item === null ? null : (
                    <li key={i}>
                      <Entry name={itemLabel(item, i)} value={item} path={`${path}[${i}]`} redacted={redacted} />
                    </li>
                  ),
                )}
              </ol>
            )}
          </dd>
        </details>
      </div>
    );
  }
  return (
    <div className="grid grid-cols-[minmax(8rem,16rem)_1fr] gap-3 border-l border-border/60 py-1 pl-3 text-sm hover:bg-raised/40">
      <dt className="truncate font-mono text-muted" title={path}>
        {name}
      </dt>
      <dd className="min-w-0">
        {Array.isArray(value) ? (
          value.length === 0 ? (
            <span className="text-muted">[]</span>
          ) : (
            <ul className="flex flex-wrap gap-1">
              {value.map((x, i) => (
                <li key={i}>
                  <Badge tone={redacted.has(`${path}[${i}]`) ? "warn" : "muted"}>{scalarText(x)}</Badge>
                </li>
              ))}
            </ul>
          )
        ) : (
          <Scalar value={value} redacted={redacted.has(path)} />
        )}
      </dd>
    </div>
  );
}

function Summary({ c }: { c: Configuration }) {
  const { t } = useI18n();
  return (
    <div className="card grid gap-4 p-4 sm:grid-cols-[1fr_2fr]">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted">{t("config.version")}</dt>
        <dd className="font-mono">{c.version || "—"}</dd>
        <dt className="text-muted">{t("config.executor")}</dt>
        <dd className="font-mono">{c.executor || "—"}</dd>
        <dt className="text-muted">{t("config.redacted")}</dt>
        <dd>{c.redacted.length > 0 ? <Badge tone="warn" icon={Lock}>{c.redacted.length}</Badge> : t("common.none")}</dd>
      </dl>
      <div>
        <h3 className="text-xs uppercase tracking-wider text-muted">{t("config.files")}</h3>
        {c.files.length === 0 ? (
          <p className="mt-1 text-sm text-muted">{t("config.noFiles")}</p>
        ) : (
          <ol className="mt-1 flex flex-col gap-1 text-sm">
            {c.files.map((f, i) => (
              <li key={f} className="flex items-center gap-2">
                <span className="w-5 text-right font-mono text-xs text-muted">{i + 1}.</span>
                <FileText className="h-4 w-4 shrink-0 text-muted" aria-hidden />
                <span className="break-all font-mono">{f}</span>
              </li>
            ))}
          </ol>
        )}
        {c.files.length > 1 && <p className="mt-2 text-xs text-muted">{t("config.layers")}</p>}
      </div>
    </div>
  );
}

export function ConfigurationPage() {
  const { t } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["configuration"],
    queryFn: async (): Promise<Configuration> => unwrap(await api.GET("/api/v1/configuration")),
    staleTime: 60_000,
  });
  const [query, setQuery] = useParam("q");
  const [all, setAll] = useParam("all");
  const [view, setView] = useParam("view");
  const [section, setSection] = useParam("section");
  const clear = useClearParams("q", "all", "section");
  const redacted = useMemo(() => new Set(q.data?.redacted ?? []), [q.data]);
  const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const pruned = useMemo(() => (q.data ? ((prune(q.data.config, "", words, !all) as Obj | undefined) ?? {}) : {}), [q.data, query, all]); // eslint-disable-line react-hooks/exhaustive-deps
  const sections = Object.keys(q.data?.config ?? {}).sort();
  const shown = Object.keys(pruned)
    .filter((s) => !section || s === section)
    .sort();
  const json = useMemo(() => JSON.stringify(q.data?.config ?? {}, null, 2), [q.data]);

  return (
    <>
      <PageHeader
        title={t("config.title")}
        lead={t("config.lead")}
        actions={
          <Button icon={RefreshCw} onClick={() => void q.refetch()} busy={q.isRefetching}>
            {t("common.refresh")}
          </Button>
        }
      />
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <div className="flex flex-col gap-6">
          <Summary c={q.data} />
          <FilterBar
            active={Boolean(query || all || section)}
            onReset={clear}
          >
            <SearchFilter label={t("filters.search")} placeholder={t("config.filter.placeholder")} value={query} onChange={setQuery} />
            <ToggleFilter label={t("config.filter.empty")} checked={Boolean(all)} onChange={(v) => setAll(v ? "1" : "")} />
            <div className="flex h-10 overflow-hidden rounded-lg border border-border text-sm" role="group" aria-label={t("config.view")}>
              {(["tree", "json"] as const).map((v) => (
                <button
                  key={v}
                  type="button"
                  aria-pressed={(view || "tree") === v}
                  onClick={() => setView(v === "tree" ? "" : v)}
                  className={clsx("px-3", (view || "tree") === v ? "bg-accent/10 font-medium text-accent" : "text-muted hover:bg-raised/70 hover:text-fg")}
                >
                  {t(v === "tree" ? "config.view.tree" : "config.view.json")}
                </button>
              ))}
            </div>
          </FilterBar>
          {view === "json" ? (
            <Section title={t("config.view.json")} actions={<CopyButton value={json} label={t("config.copy")} />}>
              <pre className="max-h-[70vh] overflow-auto rounded-lg bg-raised/50 p-4 font-mono text-xs">{json}</pre>
            </Section>
          ) : (
            <div className="grid gap-6 lg:grid-cols-[12rem_1fr]">
              <nav aria-label={t("config.sections")} className="lg:sticky lg:top-20 lg:self-start">
                <ul className="flex flex-wrap gap-1 lg:flex-col">
                  {sections.map((s) => {
                    const has = s in pruned;
                    return (
                      <li key={s}>
                        <button
                          type="button"
                          onClick={() => setSection(section === s ? "" : s)}
                          aria-pressed={section === s}
                          disabled={!has}
                          className={clsx(
                            "w-full rounded-lg px-3 py-1.5 text-left font-mono text-sm",
                            section === s ? "bg-accent/10 font-medium text-accent" : "text-muted hover:bg-raised/70 hover:text-fg",
                            !has && "cursor-default opacity-40 hover:bg-transparent hover:text-muted",
                          )}
                        >
                          {s}
                        </button>
                      </li>
                    );
                  })}
                </ul>
              </nav>
              <div className="flex min-w-0 flex-col gap-4">
                {shown.length === 0 ? (
                  <EmptyState>{t("filters.none")}</EmptyState>
                ) : (
                  shown.map((s) => {
                    const v = pruned[s];
                    return (
                      <Section key={s} title={s} id={`config-${s}`}>
                        {isObj(v) ? <Tree value={v} path={s} redacted={redacted} /> : <Entry name={s} value={v} path={s} redacted={redacted} />}
                      </Section>
                    );
                  })
                )}
              </div>
            </div>
          )}
        </div>
      )}
    </>
  );
}
