import type { components } from "@/api/schema";

export type UIConfig = components["schemas"]["UIConfig"];

// loadConfig legge la configurazione pubblica della UI (modalità di login, client OIDC),
// servita da manent-server senza autenticazione.
export async function loadConfig(): Promise<UIConfig> {
  const res = await fetch("/ui/config.json", { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw new Error(`/ui/config.json: HTTP ${res.status}`);
  }
  return (await res.json()) as UIConfig;
}
