import { useQuery } from "@tanstack/react-query";
import { Ban, RefreshCw } from "lucide-react";
import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router";

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

type Apps = TenantInfo["applications"];

/** Owned lists the applications of a tenant with their tags. */
function Owned({ apps }: { apps?: Apps }) {
  const entries = Object.entries(apps ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return <span className="text-muted">—</span>;
  return (
    <ul className="flex flex-col gap-1">
      {entries.map(([app, a]) => (
        <li key={app} className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{app}</span>
          {Object.keys(a?.tags ?? {}).length > 0 && <Tags tags={a?.tags} />}
        </li>
      ))}
    </ul>
  );
}

/** TenantsPage lists the tenant registry: the teams and the applications they own. */
export function TenantsPage() {
  const { t } = useI18n();
  const api = useApi();
  const [params, setParams] = useSearchParams();
  const filters = {
    selector: params.get("selector") ?? "",
    application: params.get("application") ?? "",
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
      <form onSubmit={submit} className="card mb-4 grid gap-3 p-4 sm:grid-cols-[2fr_1fr_auto] sm:items-end" aria-label={t("common.filters")}>
        <Field label={t("tenants.filter.selector")} hint={t("tenants.filter.selectorHint")}>
          {(p) => <input {...p} className={inputClass} value={draft.selector} placeholder="division=commerce" onChange={(e) => setDraft({ ...draft, selector: e.target.value })} />}
        </Field>
        <Field label={t("tenants.filter.application")}>
          {(p) => <input {...p} className={inputClass} value={draft.application} onChange={(e) => setDraft({ ...draft, application: e.target.value })} />}
        </Field>
        <div className="flex gap-2">
          <Button type="submit" variant="primary">
            {t("common.apply")}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              setDraft({ selector: "", application: "" });
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
                <th scope="col" className={th}>{t("tenants.col.queue")}</th>
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
                    <Owned apps={tn.applications} />
                  </td>
                  <td className={`${td} font-mono text-xs`}>{tn.queue || <span className="text-muted">{t("tenants.defaultQueue")}</span>}</td>
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

/** TenantDetailPage shows a tenant: tags, queue, the applications it owns with their tags and
 * the releases in service of each application artifact. */
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

  if (tenant.isPending) return <Loading />;
  if (tenant.isError) return <ErrorState error={tenant.error} onRetry={() => void tenant.refetch()} />;
  const tn = tenant.data;
  const apps = Object.entries(tn.applications ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const entries = [...(inventory.data ?? [])].sort((a, b) =>
    `${a.application}/${a.artifact}/${a.environment}`.localeCompare(`${b.application}/${b.artifact}/${b.environment}`),
  );
  return (
    <>
      <PageHeader title={tn.name} lead={tn.description || t("tenants.detailLead")} />
      <div className="flex flex-col gap-6">
        <section className="card p-4">
          <dl className="grid grid-cols-1 gap-4 sm:grid-cols-4">
            <Fact label={t("tenants.col.tags")}>
              <Tags tags={tn.tags} />
            </Fact>
            <Fact label={t("tenants.state")}>{tn.disabled ? <Badge tone="muted">{t("tenants.disabled")}</Badge> : <Badge tone="ok">{t("tenants.enabled")}</Badge>}</Fact>
            <Fact label={t("tenants.col.queue")}>
              <span className="font-mono text-sm">{tn.queue || t("tenants.defaultQueue")}</span>
            </Fact>
            <Fact label={t("tenants.source")}>
              <span className="font-mono text-sm">
                {tn.source || "—"}
                {tn.revision && ` @ ${tn.revision}`}
              </span>
            </Fact>
          </dl>
        </section>
        <Section title={t("tenants.col.applications")}>
          {apps.length === 0 ? (
            <EmptyState>{t("tenants.noApplications")}</EmptyState>
          ) : (
            <ul className="flex flex-col gap-2">
              {apps.map(([app, a]) => (
                <li key={app} className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{app}</span>
                  <Tags tags={a?.tags} />
                  <Link to={`/deployments?application=${encodeURIComponent(app)}`} className="text-xs hover:text-accent hover:underline">
                    {t("tenants.history")}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Section>
        <Section title={t("tenants.releases")}>
          {entries.length === 0 ? (
            <EmptyState>{inventory.isPending ? "…" : t("tenants.neverReleased")}</EmptyState>
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
                  {entries.map((e) => (
                    <tr key={`${e.application}/${e.artifact}/${e.environment}`} className="border-b border-border/40 align-top last:border-0">
                      <th scope="row" className={`${td} font-normal`}>
                        <span className="font-medium">{e.application}</span> <span className="font-mono text-xs text-muted">{e.artifact}</span>{" "}
                        <span aria-hidden>→</span>
                        <span className="sr-only"> / </span> {e.environment}
                      </th>
                      <td className={`${td} font-mono`}>
                        {e.current?.version ?? "—"}
                        {e.out_of_sync && (
                          <span className="ml-2">
                            <Badge tone="warn">{t("tenants.outOfSync")}</Badge>
                          </span>
                        )}
                      </td>
                      <td className={td}>
                        {e.current ? (
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
                        {e.current?.updated_at ? (
                          <time dateTime={e.current.updated_at} title={formatDateTime(e.current.updated_at, locale)}>
                            {formatRelative(e.current.updated_at, locale)}
                          </time>
                        ) : (
                          "—"
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {inventory.isError && <ErrorState error={inventory.error} onRetry={() => void inventory.refetch()} />}
        </Section>
      </div>
    </>
  );
}
