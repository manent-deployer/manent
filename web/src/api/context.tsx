import { useQuery } from "@tanstack/react-query";
import { createContext, useContext, useMemo, type ReactNode } from "react";

import { useAuth } from "@/auth/auth";
import type { UIConfig } from "@/lib/config";

import { createApi, unwrap, type Api, type Identity } from "./client";

interface ApiValue {
  api: Api;
  config: UIConfig;
}

const ApiContext = createContext<ApiValue | null>(null);

export function ApiProvider({ config, children, api: injected }: { config: UIConfig; children: ReactNode; api?: Api }) {
  const auth = useAuth();
  const api = useMemo(() => injected ?? createApi({ token: auth.token, expired: auth.expired }), [injected, auth.token, auth.expired]);
  const value = useMemo(() => ({ api, config }), [api, config]);
  return <ApiContext.Provider value={value}>{children}</ApiContext.Provider>;
}

export function useApi(): Api {
  const ctx = useContext(ApiContext);
  if (!ctx) throw new Error("useApi fuori da ApiProvider");
  return ctx.api;
}

export function useUIConfig(): UIConfig {
  const ctx = useContext(ApiContext);
  if (!ctx) throw new Error("useUIConfig fuori da ApiProvider");
  return ctx.config;
}

/** useMe legge identità e permessi dell'utente (GET /api/v1/me). */
export function useMe() {
  const api = useApi();
  return useQuery({
    queryKey: ["me"],
    queryFn: async (): Promise<Identity> => unwrap(await api.GET("/api/v1/me")),
    staleTime: 5 * 60_000,
  });
}
