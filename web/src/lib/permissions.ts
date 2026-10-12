import type { Identity } from "@/api/client";

/** Azioni RBAC, come in internal/authz. */
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

/** glob confronta un valore con un pattern in cui "*" vale qualsiasi sequenza (come internal/glob). */
export function glob(pattern: string, value: string): boolean {
  const re = new RegExp("^" + pattern.split("*").map((p) => p.replace(/[.+?^${}()|[\]\\]/g, "\\$&")).join(".*") + "$");
  return re.test(value);
}

/**
 * can dice se l'identità può eseguire l'azione sull'applicazione e sull'ambiente. Serve solo a
 * mostrare o nascondere le azioni: il server verifica comunque ogni richiesta.
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

/** canInApp dice se l'identità ha l'azione sull'applicazione in almeno un ambiente. */
export function canInApp(me: Identity | undefined, action: Action, application: string): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => (g.actions.includes("*") || g.actions.includes(action)) && g.applications.some((p) => glob(p, application)));
}

/** canSomewhere dice se l'identità ha l'azione su almeno un'applicazione/ambiente. */
export function canSomewhere(me: Identity | undefined, action: Action): boolean {
  if (!me) return false;
  if (!me.auth_enabled) return true;
  return me.grants.some((g) => g.actions.includes("*") || g.actions.includes(action));
}
