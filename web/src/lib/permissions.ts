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
  | "platform.read";

/** glob matches a value against a pattern where "*" stands for any sequence (as internal/glob). */
export function glob(pattern: string, value: string): boolean {
  const re = new RegExp("^" + pattern.split("*").map((p) => p.replace(/[.+?^${}()|[\]\\]/g, "\\$&")).join(".*") + "$");
  return re.test(value);
}

/**
 * can says whether the identity can perform the action on the application and environment.
 * It only shows or hides actions: the server checks every request anyway.
 */
export function can(me: Identity | undefined, action: Action, application = "*", environment = "*"): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some(
    (g) =>
      (g.actions.includes("*") || g.actions.includes(action)) &&
      g.applications.some((p) => glob(p, application)) &&
      g.environments.some((p) => glob(p, environment)),
  );
}

/** canInApp says whether the identity has the action on the application in at least one environment. */
export function canInApp(me: Identity | undefined, action: Action, application: string): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => (g.actions.includes("*") || g.actions.includes(action)) && g.applications.some((p) => glob(p, application)));
}

/** canSomewhere says whether the identity has the action on at least one application/environment. */
export function canSomewhere(me: Identity | undefined, action: Action): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => g.actions.includes("*") || g.actions.includes(action));
}
