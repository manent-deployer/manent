import type { Identity } from "@/api/client";

/** RBAC actions, as in internal/authz. */
export type Action =
  | "deployment.read"
  | "deployment.submit"
  | "deployment.dry-run"
  | "deployment.cancel"
  | "deployment.rollback"
  | "deployment.approve"
  | "deployment.freeze-override"
  | "schedule.read"
  | "schedule.write"
  | "platform.read"
  | "policy.read"
  | "policy.write"
  | "policy.approve"
  | "freeze.write";

/** glob matches a value against a pattern where "*" stands for any sequence (as internal/glob). */
export function glob(pattern: string, value: string): boolean {
  const re = new RegExp("^" + pattern.split("*").map((p) => p.replace(/[.+?^${}()|[\]\\]/g, "\\$&")).join(".*") + "$");
  return re.test(value);
}

/**
 * appMatches applies a qualified application pattern, as tenant.AppMatches: "<tenant>/<app>"
 * (globs on both parts) for the applications of the matching tenants, a pattern without "/"
 * only for the untenanted ones. With the tenant unknown only the application part is compared.
 * An application "<tenant>/<app>" (a freeze pattern) carries its tenant.
 */
export function appMatches(pattern: string, application: string, tenant?: string): boolean {
  if (tenant === undefined && application.includes("/")) {
    const i = application.indexOf("/");
    return appMatches(pattern, application.slice(i + 1), application.slice(0, i));
  }
  const i = pattern.indexOf("/");
  const tp = i < 0 ? undefined : pattern.slice(0, i);
  const ap = i < 0 ? pattern : pattern.slice(i + 1);
  if (tenant === undefined) return glob(ap, application);
  if (tp === undefined) return tenant === "untenanted" && glob(ap, application);
  return glob(tp, tenant) && glob(ap, application);
}

/**
 * can says whether the identity can perform the action on the application of the tenant and the
 * environment. It only shows or hides actions: the server checks every request anyway.
 */
export function can(me: Identity | undefined, action: Action, application = "*", environment = "*", tenant?: string): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some(
    (g) =>
      (g.actions.includes("*") || g.actions.includes(action)) &&
      g.applications.some((p) => appMatches(p, application, tenant)) &&
      g.environments.some((p) => glob(p, environment)),
  );
}

/** canInApp says whether the identity has the action on the application in at least one environment. */
export function canInApp(me: Identity | undefined, action: Action, application: string, tenant?: string): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => (g.actions.includes("*") || g.actions.includes(action)) && g.applications.some((p) => appMatches(p, application, tenant)));
}

/** canSomewhere says whether the identity has the action on at least one application/environment. */
export function canSomewhere(me: Identity | undefined, action: Action): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => g.actions.includes("*") || g.actions.includes(action));
}
