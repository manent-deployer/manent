import type { components } from "@/api/schema";

export type UIConfig = components["schemas"]["UIConfig"];

// loadConfig reads the public UI configuration (login mode, OIDC client), served by
// manent-server without authentication.
export async function loadConfig(): Promise<UIConfig> {
  const res = await fetch("/ui/config.json", { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw new Error(`/ui/config.json: HTTP ${res.status}`);
  }
  return (await res.json()) as UIConfig;
}
