import { useMutation } from "@tanstack/react-query";
import { UserSearch } from "lucide-react";
import { useState, type FormEvent } from "react";

import { unwrap, type Grant, type PolicyAccessRequest as AccessRequest, type PolicyRevision } from "@/api/client";
import { useApi } from "@/api/context";
import { Badge, Button, ErrorState, Field, inputClass } from "@/components/ui";
import type { MessageKey } from "@/i18n/en";
import { useI18n } from "@/i18n/i18n";
import { formatDateTime } from "@/lib/format";

/** splitList reads a comma separated list, without empty items. */
function splitList(v: string): string[] {
  return v
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

/** parseClaims reads "key=value" pairs separated by commas. */
function parseClaims(v: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const item of splitList(v)) {
    const i = item.indexOf("=");
    if (i > 0) out[item.slice(0, i).trim()] = item.slice(i + 1).trim();
  }
  return out;
}

function GrantsTable({ grants, caption, sign }: { grants: Grant[]; caption: string; sign?: "+" | "-" }) {
  const { t, locale } = useI18n();
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[40rem] text-left text-sm">
        <caption className="sr-only">{caption}</caption>
        <thead className="border-b border-border/70 text-xs uppercase tracking-wider text-muted">
          <tr>
            <th scope="col" className="py-2 pr-4 font-medium">{t("policies.access.grant")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("policies.access.actions")}</th>
            <th scope="col" className="py-2 pr-4 font-medium">{t("policies.access.scope")}</th>
            <th scope="col" className="py-2 font-medium">{t("policies.access.until")}</th>
          </tr>
        </thead>
        <tbody>
          {grants.map((g, i) => (
            <tr key={`${g.name}-${i}`} className="border-b border-border/40 align-top last:border-0">
              <th scope="row" className="py-2 pr-4 font-normal">
                {sign && <span className={sign === "+" ? "text-ok" : "text-bad"}>{sign} </span>}
                {g.name || "—"}
              </th>
              <td className="py-2 pr-4 font-mono text-xs">{g.role || g.actions.join(", ")}</td>
              <td className="py-2 pr-4 font-mono text-xs">
                {g.applications.join(",")} / {g.environments.join(",")}
                {g.tenants && <span className="text-muted"> · {g.tenants}</span>}
                {g.selector && <span className="text-muted"> · {g.selector}</span>}
              </td>
              <td className="py-2 text-muted">{g.until ? <time dateTime={g.until}>{formatDateTime(g.until, locale)}</time> : "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** PolicyAccess simulates with a revision the permissions of an identity typed by hand. */
export function PolicyAccess({ revisions }: { revisions: PolicyRevision[] }) {
  const { t } = useI18n();
  const api = useApi();
  const [revision, setRevision] = useState("current");
  const [subject, setSubject] = useState("");
  const [groups, setGroups] = useState("");
  const [claims, setClaims] = useState("");
  const [compare, setCompare] = useState(true);
  const [action, setAction] = useState("");
  const [application, setApplication] = useState("");
  const [environment, setEnvironment] = useState("");
  const [artifact, setArtifact] = useState("");
  const run = useMutation({
    mutationFn: async (body: AccessRequest) =>
      unwrap(await api.POST("/api/v1/policies/{revision}/access", { params: { path: { revision } }, body })),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const body: AccessRequest = { subject: subject.trim(), groups: splitList(groups), claims: parseClaims(claims), compare: compare && revision !== "current" };
    if (action) body.check = { action, application: application.trim(), environment: environment.trim(), artifact: artifact.trim() || undefined };
    run.mutate(body);
  };
  const r = run.data;
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted">{t("policies.access.lead")}</p>
      <form className="grid gap-3 sm:grid-cols-2" onSubmit={submit}>
        <Field label={t("policies.access.revision")}>
          {(p) => (
            <select {...p} className={inputClass} value={revision} onChange={(e) => setRevision(e.target.value)}>
              <option value="current">{t("policies.access.current")}</option>
              {revisions.map((x) => (
                <option key={x.revision} value={String(x.revision)}>
                  {t("policies.revision", { revision: x.revision })} · {t(`policies.status.${x.status}` as MessageKey)}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label={t("policies.access.subject")} hint={t("policies.access.subject.hint")}>
          {(p) => <input {...p} className={inputClass} required value={subject} onChange={(e) => setSubject(e.target.value)} placeholder="oidc:alice" />}
        </Field>
        <Field label={t("policies.access.groups")} hint={t("policies.access.list.hint")}>
          {(p) => <input {...p} className={inputClass} value={groups} onChange={(e) => setGroups(e.target.value)} placeholder="team-shop, oncall" />}
        </Field>
        <Field label={t("policies.access.claims")} hint={t("policies.access.claims.hint")}>
          {(p) => <input {...p} className={inputClass} value={claims} onChange={(e) => setClaims(e.target.value)} placeholder="employee_type=contractor" />}
        </Field>
        <Field label={t("policies.access.action")} hint={t("policies.access.action.hint")}>
          {(p) => <input {...p} className={inputClass} value={action} onChange={(e) => setAction(e.target.value)} placeholder="deployment.submit" />}
        </Field>
        <div className="grid grid-cols-3 gap-2">
          <Field label={t("policies.access.application")}>
            {(p) => <input {...p} className={inputClass} value={application} onChange={(e) => setApplication(e.target.value)} required={!!action} />}
          </Field>
          <Field label={t("policies.access.environment")}>
            {(p) => <input {...p} className={inputClass} value={environment} onChange={(e) => setEnvironment(e.target.value)} required={!!action} />}
          </Field>
          <Field label={t("policies.access.artifact")}>
            {(p) => <input {...p} className={inputClass} value={artifact} onChange={(e) => setArtifact(e.target.value)} />}
          </Field>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={compare} disabled={revision === "current"} onChange={(e) => setCompare(e.target.checked)} />
          {t("policies.access.compare")}
        </label>
        <div className="flex justify-end">
          <Button type="submit" variant="primary" icon={UserSearch} busy={run.isPending}>
            {t("policies.access.run")}
          </Button>
        </div>
      </form>
      {run.isError && <ErrorState error={run.error} />}
      {r && (
        <div className="flex flex-col gap-3" aria-live="polite">
          <p className="text-sm">{t("policies.access.result", { subject: r.subject, revision: r.revision })}</p>
          {r.grants.length === 0 ? (
            <p className="text-sm text-muted">{t("policies.access.none")}</p>
          ) : (
            <GrantsTable grants={r.grants} caption={t("policies.access.grants")} />
          )}
          {(r.warnings ?? []).map((w) => (
            <p key={w} className="text-xs text-warn">
              {t("policies.access.discarded", { detail: w })}
            </p>
          ))}
          {r.compared && (
            <>
              <p className="text-sm">{t("policies.access.compared", { revision: r.compared })}</p>
              {(r.gained ?? []).length + (r.lost ?? []).length === 0 ? (
                <p className="text-sm text-muted">{t("policies.access.noDiff")}</p>
              ) : (
                <>
                  {(r.gained ?? []).length > 0 && <GrantsTable grants={r.gained ?? []} caption={t("policies.access.gained")} sign="+" />}
                  {(r.lost ?? []).length > 0 && <GrantsTable grants={r.lost ?? []} caption={t("policies.access.lost")} sign="-" />}
                </>
              )}
            </>
          )}
          {r.check && (
            <div className="flex flex-col gap-1">
              <div>
                <Badge tone={r.check.allowed ? "ok" : "bad"}>{t(r.check.allowed ? "policies.access.allowed" : "policies.access.denied")}</Badge>
                {r.check.granted_by && <span className="ml-2 text-sm text-muted">{t("policies.access.grantedBy", { grant: r.check.granted_by })}</span>}
              </div>
              {(r.check.denied ?? []).map((d) => (
                <p key={d} className="text-sm">
                  – {d}
                </p>
              ))}
              {r.check.reason && <p className="text-sm text-muted">{r.check.reason}</p>}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
