import createClient from "openapi-fetch";

import type { Api, Identity, Status } from "@/api/client";
import type { paths } from "@/api/schema";

export interface Call {
  method: string;
  path: string;
  body?: unknown;
}

type Handler = (req: { method: string; path: string; query: URLSearchParams; body?: unknown }) => { status?: number; json?: unknown } | undefined;

/** fakeApi creates an API client that answers with the given data and records the calls. */
export function fakeApi(handler: Handler): { api: Api; calls: Call[] } {
  const calls: Call[] = [];
  const fetch = async (input: Request | string | URL, init?: RequestInit): Promise<Response> => {
    const req = input instanceof Request ? input : new Request(input, init);
    const url = new URL(req.url);
    const text = req.method === "GET" ? "" : await req.text();
    const body = text ? (JSON.parse(text) as unknown) : undefined;
    calls.push({ method: req.method, path: url.pathname, body });
    const res = handler({ method: req.method, path: url.pathname, query: url.searchParams, body }) ?? { status: 404, json: { error: "not found" } };
    const status = res.status ?? 200;
    return new Response(status === 204 ? null : JSON.stringify(res.json ?? {}), { status, headers: { "Content-Type": "application/json" } });
  };
  return { api: createClient<paths>({ baseUrl: "http://manent.test", fetch }) as Api, calls };
}

export const now = "2026-10-11T10:00:00Z";

export function dep(over: Partial<Status>): Status {
  return {
    id: "dep-1",
    state: "SUCCESS",
    application: "shop",
    environment: "prod",
    artifact: "shop",
    version: "1.4.0",
    kind: "deploy",
    requested_by: "oidc:alice",
    created_at: now,
    updated_at: now,
    ...over,
  };
}

export function identity(over: Partial<Identity> = {}): Identity {
  return {
    subject: "oidc:carol",
    name: "carol",
    method: "oidc",
    groups: ["cab"],
    auth_enabled: true,
    grants: [{ role: "approver", actions: ["deployment.read", "schedule.read", "deployment.approve"], applications: ["*"], environments: ["*"] }],
    ...over,
  };
}
