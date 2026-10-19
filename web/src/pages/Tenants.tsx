import { useQuery } from "@tanstack/react-query";
import { Ban, RefreshCw } from "lucide-react";
import { useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { unwrap, type InventoryEntry, type TenantInfo } from "@/api/client";
import { useApi } from "@/api/context";
import { Badge, Button, EmptyState, ErrorState, Field, Loading, PageHeader, Section, StatusBadge, inputClass } from "@/components/ui";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime, formatRelative, shortId } from "@/lib/format";

const th = "px-4 py-3 font-medium";
const td = "px-4 py-3";

function Tags({ tags }: { tags?: Record<string, string> }) {
  const entries = Object.entries(tags ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return <span className="text-muted">—</span>;
  return (
    <ul className="flex flex-wrap gap-1">
      {entries.map(([k, v]) => (
        <li key={k}>
          <Badge tone="violet">
            {k}={v}
          </Badge>
        </li>
      ))}
    </ul>
  );
}

function Connected({ apps }: { apps?: Record<string, string[]> }) {
  const entries = Object.entries(apps ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return <span className="text-muted">—</span>;
  return (
    <ul className="flex flex-col gap-0.5">
      {entries.map(([app, envs]) => (
        <li key={app}>
          <span className="font-medium">{app}</span> <span className="text-muted">→ {envs.join(", ")}</span>
        </li>
      ))}
    </ul>
  );
}

/** TenantsPage lists the tenant registry, filtered by selector, application and environment. */
export function TenantsPage() {
  const { t } = useI18n();
  const api = useApi();
  const [params, setParams] = useSearchParams();
  const filters = {
    selector: params.get("selector") ?? "",
    application: params.get("application") ?? "",
    environment: params.get("environment") ?? "",
  };
  const [draft, setDraft] = useState(filters);
  const q = useQuery({
    queryKey: ["tenants", filters],
    queryFn: async (): Promise<TenantInfo[]> => {
      const query: Record<string, string> = {};
      for (const [k, v] of Object.entries(filters)) if (v) query[k] = v;
      return unwrap(await api.GET("/api/v1/tenants", { params: { query } })).tenants;
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = new URLSearchParams();
    for (const [k, v] of Object.entries(draft)) if (v.trim()) next.set(k, v.trim());
    setParams(next);
  };
  return (
    <>
      <PageHeader
        title={t("tenants.title")}
        lead={t("tenants.lead")}
        actions={
          <Button icon={RefreshCw} onClick={() => void q.refetch()} busy={q.isRefetching}>
            {t("common.refresh")}
          </Button>
        }
      />
      <form onSubmit={submit} className="card mb-4 grid gap-3 p-4 sm:grid-cols-[2fr_1fr_1fr_auto] sm:items-end" aria-label={t("common.filters")}>
        <Field label={t("tenants.filter.selector")} hint={t("tenants.filter.selectorHint")}>
          {(p) => <input {...p} className={inputClass} value={draft.selector} placeholder="tier in (gold, silver)" onChange={(e) => setDraft({ ...draft, selector: e.target.value })} />}
        </Field>
        <Field label={t("deployments.filter.application")}>
          {(p) => <input {...p} className={inputClass} value={draft.application} onChange={(e) => setDraft({ ...draft, application: e.target.value })} />}
        </Field>
        <Field label={t("deployments.filter.environment")}>
          {(p) => <input {...p} className={inputClass} value={draft.environment} onChange={(e) => setDraft({ ...draft, environment: e.target.value })} />}
        </Field>
        <div className="flex gap-2">
          <Button type="submit" variant="primary">
            {t("common.apply")}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              setDraft({ selector: "", application: "", environment: "" });
              setParams(new URLSearchParams());
            }}
          >
            {t("common.reset")}
          </Button>
        </div>
      </form>
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.data.length === 0 ? (
        <EmptyState>{t("tenants.empty")}</EmptyState>
      ) : (
        <div className="card overflow-x-auto">
          <table className="w-full min-w-[48rem] text-left text-sm">
            <caption className="sr-only">{t("tenants.title")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className={th}>{t("tenants.col.tenant")}</th>
                <th scope="col" className={th}>{t("tenants.col.tags")}</th>
                <th scope="col" className={th}>{t("tenants.col.applications")}</th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((tn) => (
                <tr key={tn.name} className="border-b border-border/40 align-top last:border-0 hover:bg-raised/40">
                  <th scope="row" className={`${td} font-normal`}>
                    <Link to={`/tenants/${encodeURIComponent(tn.name)}`} className="font-mono font-medium text-fg underline-offset-4 hover:text-accent hover:underline">
                      {tn.name}
                    </Link>
                    {tn.disabled && (
                      <span className="ml-2">
                        <Badge tone="muted" icon={Ban}>
                          {t("tenants.disabled")}
                        </Badge>
                      </span>
                    )}
                    {tn.description && <div className="text-xs text-muted">{tn.description}</div>}
                  </th>
                  <td className={td}>
                    <Tags tags={tn.tags} />
                  </td>
                  <td className={td}>
                    <Connected apps={tn.applications} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wider text-muted">{label}</dt>
      <dd className="mt-0.5 break-words">{children}</dd>
    </div>
  );
}

/** TenantDetailPage shows a tenant: tags, connected environments with the release in service,
 * parameters and secret names per application. */
export function TenantDetailPage() {
  const { name = "" } = useParams();
  const { t, locale } = useI18n();
  const api = useApi();
  const tenant = useQuery({
    queryKey: ["tenant", name],
    queryFn: async (): Promise<TenantInfo> => unwrap(await api.GET("/api/v1/tenants/{name}", { params: { path: { name } } })),
  });
  const inventory = useQuery({
    queryKey: ["inventory", { tenant: name }],
    queryFn: async (): Promise<InventoryEntry[]> => unwrap(await api.GET("/api/v1/inventory", { params: { query: { tenant: name } } })).inventory,
  });
  const byKey = useMemo(() => new Map((inventory.data ?? []).map((e) => [`${e.application}/${e.environment}`, e])), [inventory.data]);

  if (tenant.isPending) return <Loading />;
  if (tenant.isError) return <ErrorState error={tenant.error} onRetry={() => void tenant.refetch()} />;
  const tn = tenant.data;
  const apps = Object.entries(tn.applications ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <>
      <PageHeader title={tn.name} lead={tn.description || t("tenants.detailLead")} />
      <div className="flex flex-col gap-6">
        <section className="card p-4">
          <dl className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <Fact label={t("tenants.col.tags")}>
              <Tags tags={tn.tags} />
            </Fact>
            <Fact label={t("tenants.state")}>{tn.disabled ? <Badge tone="muted">{t("tenants.disabled")}</Badge> : <Badge tone="ok">{t("tenants.enabled")}</Badge>}</Fact>
            <Fact label={t("tenants.source")}>
              <span className="font-mono text-sm">{tn.source || "—"}</span>
            </Fact>
          </dl>
        </section>
        <Section title={t("tenants.releases")}>
          {apps.length === 0 ? (
            <EmptyState>{t("tenants.noApplications")}</EmptyState>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[44rem] text-left text-sm">
                <caption className="sr-only">{t("tenants.releases")}</caption>
                <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
                  <tr>
                    <th scope="col" className={th}>{t("tenants.col.applicationEnv")}</th>
                    <th scope="col" className={th}>{t("tenants.col.version")}</th>
                    <th scope="col" className={th}>{t("tenants.col.deployment")}</th>
                    <th scope="col" className={th}>{t("tenants.col.updated")}</th>
                  </tr>
                </thead>
                <tbody>
                  {apps.flatMap(([app, envs]) =>
                    envs.map((env) => {
                      const e = byKey.get(`${app}/${env}`);
                      return (
                        <tr key={`${app}/${env}`} className="border-b border-border/40 align-top last:border-0">
                          <th scope="row" className={`${td} font-normal`}>
                            <span className="font-medium">{app}</span> <span aria-hidden>→</span>
                            <span className="sr-only"> / </span> {env}
                          </th>
                          <td className={`${td} font-mono`}>
                            {e?.current?.version ?? <span className="text-muted">{inventory.isPending ? "…" : t("tenants.neverReleased")}</span>}
                            {e?.out_of_sync && (
                              <span className="ml-2">
                                <Badge tone="warn">{t("tenants.outOfSync")}</Badge>
                              </span>
                            )}
                          </td>
                          <td className={td}>
                            {e?.current ? (
                              <span className="flex flex-wrap items-center gap-2">
                                <Link to={`/deployments/${encodeURIComponent(e.current.id)}`} className="font-mono text-xs hover:text-accent hover:underline">
                                  {shortId(e.current.id)}
                                </Link>
                                <StatusBadge state={e.current.state} />
                              </span>
                            ) : (
                              "—"
                            )}
                          </td>
                          <td className={td}>
                            {e?.current?.updated_at ? (
                              <time dateTime={e.current.updated_at} title={formatDateTime(e.current.updated_at, locale)}>
                                {formatRelative(e.current.updated_at, locale)}
                              </time>
                            ) : (
                              "—"
                            )}
                          </td>
                        </tr>
                      );
                    }),
                  )}
                </tbody>
              </table>
            </div>
          )}
          {inventory.isError && <ErrorState error={inventory.error} onRetry={() => void inventory.refetch()} />}
        </Section>
        <Section title={t("tenants.configuration")}>
          {apps.length === 0 ? (
            <EmptyState>{t("tenants.noApplications")}</EmptyState>
          ) : (
            <div className="grid gap-4 lg:grid-cols-2">
              {apps.map(([app]) => {
                const params = Object.entries(tn.params?.[app] ?? {}).sort(([a], [b]) => a.localeCompare(b));
                const secrets = [...(tn.secrets?.[app] ?? [])].sort();
                return (
                  <div key={app} className="rounded-lg border border-border/60 p-4">
                    <h3 className="font-medium">{app}</h3>
                    <h4 className="mt-3 text-xs uppercase tracking-wider text-muted">{t("tenants.params")}</h4>
                    {params.length === 0 ? (
                      <p className="text-sm text-muted">—</p>
                    ) : (
                      <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 font-mono text-xs">
                        {params.map(([k, v]) => (
                          <div key={k} className="contents">
                            <dt className="text-muted">{k}</dt>
                            <dd className="break-all">{v}</dd>
                          </div>
                        ))}
                      </dl>
                    )}
                    <h4 className="mt-3 text-xs uppercase tracking-wider text-muted">{t("tenants.secrets")}</h4>
                    {secrets.length === 0 ? (
                      <p className="text-sm text-muted">—</p>
                    ) : (
                      <ul className="mt-1 flex flex-wrap gap-1">
                        {secrets.map((s) => (
                          <li key={s}>
                            <Badge>{s}</Badge>
                          </li>
                        ))}
                      </ul>
                    )}
                    <p className="mt-3 text-xs">
                      <Link to={`/deployments?application=${encodeURIComponent(app)}&tenant=${encodeURIComponent(tn.name)}`} className="hover:text-accent hover:underline">
                        {t("tenants.history")}
                      </Link>
                    </p>
                  </div>
                );
              })}
            </div>
          )}
          <p className="mt-3 text-xs text-muted">{t("tenants.secretsNote")}</p>
        </Section>
      </div>
    </>
  );
}
