import createClient, { type Middleware } from "openapi-fetch";

import type { components, paths } from "./schema";

export type Status = components["schemas"]["Status"];
export type RolloutState = components["schemas"]["RolloutState"];
export type TrafficState = components["schemas"]["TrafficState"];
export type RolloutStage = components["schemas"]["RolloutStage"];
export type ApprovalState = components["schemas"]["ApprovalState"];
export type StepRecord = components["schemas"]["StepRecord"];
export type LogLine = components["schemas"]["LogLine"];
export type InventoryEntry = components["schemas"]["InventoryEntry"];
export type WindowStatus = components["schemas"]["WindowStatus"];
export type ScheduleInfo = components["schemas"]["ScheduleInfo"];
export type DORAReport = components["schemas"]["DORAReport"];
export type DORAMetrics = components["schemas"]["DORAMetrics"];
export type Identity = components["schemas"]["Identity"];
export type Submission = components["schemas"]["Submission"];
export type Activity = components["schemas"]["Activity"];
export type Worker = components["schemas"]["Worker"];
export type AfterState = components["schemas"]["AfterState"];
export type ReleaseStatus = components["schemas"]["ReleaseStatus"];

/** API error with the HTTP status and the server message. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export interface AuthHooks {
  token: () => string | undefined;
  expired: () => void;
}

export function createApi(auth: AuthHooks) {
  const client = createClient<paths>({ baseUrl: "" });
  const mw: Middleware = {
    onRequest({ request }) {
      const t = auth.token();
      if (t) request.headers.set("Authorization", `Bearer ${t}`);
      return request;
    },
    onResponse({ response }) {
      if (response.status === 401) auth.expired();
      return response;
    },
  };
  client.use(mw);
  return client;
}

export type Api = ReturnType<typeof createApi>;

/** unwrap returns the data of a response or throws ApiError with the server message. */
export function unwrap<T>(r: { data?: T; error?: unknown; response: Response }): T {
  if (r.error !== undefined || !r.response.ok) {
    const msg =
      r.error && typeof r.error === "object" && "error" in r.error ? String((r.error as { error: unknown }).error) : r.response.statusText;
    throw new ApiError(r.response.status, msg || `HTTP ${r.response.status}`);
  }
  return r.data as T;
}

/** States in which a deploy will not change any more. */
export const TERMINAL = new Set(["SUCCESS", "ERROR", "CANCELLED", "REJECTED", "EXPIRED", "DEPENDENCY_FAILED", "MAX_RECOVERY_ATTEMPTS_EXCEEDED"]);
