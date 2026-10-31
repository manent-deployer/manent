import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, History, Scale, XCircle } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router";

import { ApiError, unwrap, type PolicyRevision } from "@/api/client";
import { useApi, useMe } from "@/api/context";
import { useToast } from "@/components/Toaster";
import { Badge, Button, Dialog, EmptyState, ErrorState, Loading, PageHeader, Section } from "@/components/ui";
import type { MessageKey } from "@/i18n/en";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime } from "@/lib/format";
import { canSomewhere } from "@/lib/permissions";

import { PolicyAccess } from "./PolicyAccess";

const statusTone = { draft: "info", active: "ok", superseded: "muted" } as const;

function StatusBadge({ status }: { status: PolicyRevision["status"] }) {
  const { t } = useI18n();
  return <Badge tone={statusTone[status]}>{t(`policies.status.${status}` as MessageKey)}</Badge>;
}

/** Impact shows the real deploys of the last week whose decision would change. */
function Impact({ revision }: { revision: number }) {
  const { t, locale } = useI18n();
  const api = useApi();
  const q = useQuery({
    queryKey: ["policy-impact", revision],
    queryFn: async () =>
      unwrap(await api.GET("/api/v1/policies/{revision}/impact", { params: { path: { revision: String(revision) }, query: { since: "7d" } } })),
  });
  if (q.isPending) return <Loading />;
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const changes = q.data.changes ?? [];
  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm text-muted">{t("policies.impact.summary", { evaluated: q.data.evaluated, changed: changes.length })}</p>
      {changes.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[40rem] text-left text-sm">
            <caption className="sr-only">{t("policies.impact.caption")}</caption>
            <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
              <tr>
                <th scope="col" className="py-2 pr-4 font-medium">{t("policies.impact.deployment")}</th>
                <th scope="col" className="py-2 pr-4 font-medium">{t("policies.impact.before")}</th>
                <th scope="col" className="py-2 font-medium">{t("policies.impact.after")}</th>
              </tr>
            </thead>
            <tbody>
              {changes.map((c) => (
                <tr key={c.deployment} className="border-b border-border/40 align-top last:border-0">
                  <th scope="row" className="py-2 pr-4 font-normal">
                    <Link className="font-mono text-xs text-accent hover:underline" to={`/deployments/${encodeURIComponent(c.deployment)}`}>
                      {c.deployment}
                    </Link>
                    <p className="text-xs text-muted">
                      {c.application}/{c.environment} {c.version} · <time dateTime={c.created_at}>{formatDateTime(c.created_at, locale)}</time>
                    </p>
                  </th>
                  <td className="py-2 pr-4 font-mono text-xs">{c.before}</td>
                  <td className="py-2 font-mono text-xs">{c.after}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/** Revision shows files and tests of a revision, with approval, return and impact. */
function Revision({ revision, onClose }: { revision: number; onClose: () => void }) {
  const { t } = useI18n();
  const api = useApi();
  const me = useMe();
  const qc = useQueryClient();
  const toast = useToast();
  const [impact, setImpact] = useState(false);
  const q = useQuery({
    queryKey: ["policy", revision],
    queryFn: async () => unwrap(await api.GET("/api/v1/policies/{revision}", { params: { path: { revision: String(revision) } } })),
  });
  const activate = useMutation({
    mutationFn: async (draft: boolean) => {
      const params = { params: { path: { revision: String(revision) } } };
      if (draft) unwrap(await api.POST("/api/v1/policies/{revision}/approve", params));
      else unwrap(await api.POST("/api/v1/policies/{revision}/activate", params));
    },
    onSuccess: () => {
      toast("ok", t("policies.activated", { revision }));
      void qc.invalidateQueries({ queryKey: ["policies"] });
      void qc.invalidateQueries({ queryKey: ["policy", revision] });
    },
    onError: (e) => toast("error", t("actions.failed", { message: e instanceof Error ? e.message : String(e) })),
  });
  const canApprove = canSomewhere(me.data, "policy.approve");
  const r = q.data;
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={t("policies.revision", { revision })} description={r?.comment || undefined}>
      {q.isPending && <Loading />}
      {q.isError && <ErrorState error={q.error} onRetry={() => void q.refetch()} />}
      {r && (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <StatusBadge status={r.status} />
            <span className="text-muted">{t("policies.by", { who: r.author })}</span>
            {r.reviewed_by && <span className="text-muted">· {t("policies.reviewedBy", { who: r.reviewed_by })}</span>}
            <span className="font-mono text-xs text-muted">sha256 {r.sha256.slice(0, 12)}</span>
          </div>
          {r.tests && r.tests.length > 0 && (
            <ul className="space-y-1 text-sm" aria-label={t("policies.tests")}>
              {r.tests.map((x) => (
                <li key={x.name} className="flex items-start gap-2">
                  {x.pass ? <CheckCircle2 className="mt-0.5 h-4 w-4 text-ok" aria-hidden /> : <XCircle className="mt-0.5 h-4 w-4 text-bad" aria-hidden />}
                  <span className="font-mono text-xs">{x.name}</span>
                  <span className="sr-only">{x.pass ? t("policies.test.pass") : t("policies.test.fail")}</span>
                </li>
              ))}
            </ul>
          )}
          {Object.entries(r.files ?? {}).map(([name, body]) => (
            <figure key={name} className="flex flex-col gap-1">
              <figcaption className="font-mono text-xs text-muted">{name}</figcaption>
              <pre className="max-h-72 overflow-auto rounded-lg border border-border bg-bg/60 p-3 font-mono text-xs leading-relaxed">{body}</pre>
            </figure>
          ))}
          {impact ? (
            <Section title={t("policies.impact.title")}>
              <Impact revision={revision} />
            </Section>
          ) : (
            canSomewhere(me.data, "policy.write") && (
              <Button size="sm" icon={Scale} onClick={() => setImpact(true)}>
                {t("policies.impact.run")}
              </Button>
            )
          )}
          <div className="flex flex-wrap justify-end gap-2">
            {canApprove && r.status === "draft" && (
              <Button variant="primary" icon={CheckCircle2} busy={activate.isPending} onClick={() => activate.mutate(true)}>
                {t("policies.approve")}
              </Button>
            )}
            {canApprove && r.status === "superseded" && (
              <Button icon={History} busy={activate.isPending} onClick={() => activate.mutate(false)}>
                {t("policies.activate")}
              </Button>
            )}
          </div>
        </div>
      )}
    </Dialog>
  );
}

export function PoliciesPage() {
  const { t, locale } = useI18n();
  const api = useApi();
  const me = useMe();
  const [open, setOpen] = useState<number>();
  const q = useQuery({ queryKey: ["policies"], queryFn: async () => unwrap(await api.GET("/api/v1/policies")), refetchInterval: 30_000 });
  return (
    <>
      <PageHeader title={t("policies.title")} lead={t("policies.lead")} />
      {q.isPending && <Loading />}
      {q.isError &&
        (q.error instanceof ApiError && q.error.status === 422 ? (
          <EmptyState>{q.error.message}</EmptyState>
        ) : (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ))}
      {q.data && (
        <div className="flex flex-col gap-4">
          <Section title={t("policies.current")}>
            <dl className="grid gap-3 text-sm sm:grid-cols-3">
              <div>
                <dt className="text-xs uppercase tracking-wider text-muted">{t("policies.engine")}</dt>
                <dd className="font-mono">{q.data.engine}</dd>
              </div>
              <div>
                <dt className="text-xs uppercase tracking-wider text-muted">{t("policies.source")}</dt>
                <dd className="font-mono">{q.data.source || "—"}</dd>
              </div>
              <div>
                <dt className="text-xs uppercase tracking-wider text-muted">{t("policies.inForce")}</dt>
                <dd className="font-mono">{q.data.current || "—"}</dd>
              </div>
            </dl>
          </Section>
          <Section title={t("policies.revisions")}>
            {q.data.revisions.length === 0 ? (
              <EmptyState>{t(q.data.engine === "opa" && q.data.source === "store" ? "policies.empty" : "policies.notStore")}</EmptyState>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[44rem] text-left text-sm">
                  <caption className="sr-only">{t("policies.revisions")}</caption>
                  <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
                    <tr>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("policies.col.revision")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("policies.col.status")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("policies.col.comment")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("policies.col.author")}</th>
                      <th scope="col" className="py-2 pr-4 font-medium">{t("policies.col.tests")}</th>
                      <th scope="col" className="py-2 font-medium">{t("policies.col.created")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {q.data.revisions.map((r) => (
                      <tr key={r.revision} className="border-b border-border/40 align-top last:border-0">
                        <th scope="row" className="py-2 pr-4 font-normal">
                          <button type="button" className="font-mono font-medium text-accent hover:underline" onClick={() => setOpen(r.revision)}>
                            {r.revision}
                          </button>
                        </th>
                        <td className="py-2 pr-4">
                          <StatusBadge status={r.status} />
                        </td>
                        <td className="py-2 pr-4">{r.comment || "—"}</td>
                        <td className="py-2 pr-4 text-muted">
                          {r.author}
                          {r.reviewed_by && <p className="text-xs">{t("policies.reviewedBy", { who: r.reviewed_by })}</p>}
                        </td>
                        <td className="py-2 pr-4 text-muted">
                          {r.tests && r.tests.length > 0 ? t("policies.testsPassed", { passed: r.tests.filter((x) => x.pass).length, total: r.tests.length }) : "—"}
                        </td>
                        <td className="py-2 text-muted">
                          <time dateTime={r.created_at}>{formatDateTime(r.created_at, locale)}</time>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Section>
          {me.data?.authz_engine === "opa" && canSomewhere(me.data, "policy.write") && (
            <Section title={t("policies.access.title")} id="access">
              <PolicyAccess revisions={q.data.revisions} />
            </Section>
          )}
        </div>
      )}
      {open !== undefined && <Revision revision={open} onClose={() => setOpen(undefined)} />}
    </>
  );
}
