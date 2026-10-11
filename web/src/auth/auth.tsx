import { InMemoryWebStorage, UserManager, WebStorageStateStore, type User } from "oidc-client-ts";
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import type { UIConfig } from "@/lib/config";

// Stato del login. Con auth_mode "none" non c'è login; "unavailable" è un server con
// autenticazione attiva ma senza client OIDC per la UI.
export type AuthStatus = "loading" | "anonymous" | "authenticated" | "redirecting" | "unavailable" | "insecure" | "error";

interface AuthContextValue {
  status: AuthStatus;
  error?: string;
  /** Token da mandare all'API; undefined senza autenticazione. */
  token: () => string | undefined;
  /** Porta all'identity provider e torna sulla pagina corrente. */
  login: () => void;
  logout: () => void;
  /** Chiamato dal client API su un 401: il token non è più valido. */
  expired: () => void;
  name?: string;
}

const AuthContext = createContext<AuthContextValue | null>(null);

interface ReturnState {
  returnTo: string;
}

function userManager(cfg: UIConfig): UserManager {
  const origin = window.location.origin;
  return new UserManager({
    authority: cfg.issuer ?? "",
    client_id: cfg.client_id ?? "",
    redirect_uri: origin + (cfg.redirect_path ?? "/auth/callback"),
    post_logout_redirect_uri: origin + "/",
    response_type: "code",
    scope: (cfg.scopes ?? ["openid", "profile"]).join(" "),
    // I token restano solo in memoria: niente localStorage né cookie, quindi niente CSRF e
    // nessun token leggibile da altre schede. Lo stato del redirect (PKCE) sta in
    // sessionStorage per la durata del solo login.
    userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
    stateStore: new WebStorageStateStore({ store: window.sessionStorage }),
    automaticSilentRenew: true,
    loadUserInfo: false,
    monitorSession: false,
  });
}

export function AuthProvider({ config, children }: { config: UIConfig; children: ReactNode }) {
  const mgr = useMemo(() => (config.auth_mode === "oidc" ? userManager(config) : null), [config]);
  const navigate = useNavigate();
  const userRef = useRef<User | null>(null);
  const [status, setStatus] = useState<AuthStatus>(() => {
    if (config.auth_mode === "none") return "anonymous";
    if (config.auth_mode === "unavailable") return "unavailable";
    // PKCE usa crypto.subtle, disponibile solo in un contesto sicuro (HTTPS o localhost).
    if (!window.isSecureContext) return "insecure";
    return "loading";
  });
  const [name, setName] = useState<string>();
  const [error, setError] = useState<string>();

  const login = useCallback(() => {
    if (!mgr) return;
    setStatus("redirecting");
    const returnTo = window.location.pathname + window.location.search + window.location.hash;
    void mgr.signinRedirect({ state: { returnTo } satisfies ReturnState }).catch((e: unknown) => {
      setError(String(e));
      setStatus("error");
    });
  }, [mgr]);

  // L'avvio del login (callback o redirect) va eseguito una sola volta: l'effetto può ripartire
  // quando cambia il percorso (navigate) o, in sviluppo, per StrictMode.
  const started = useRef(false);
  useEffect(() => {
    if (!mgr || !window.isSecureContext || started.current) return;
    started.current = true;
    const set = (u: User | null) => {
      userRef.current = u;
      setName(u?.profile.preferred_username ?? u?.profile.name ?? u?.profile.sub);
    };
    mgr.events.addUserLoaded(set);
    mgr.events.addUserUnloaded(() => set(null));
    mgr.events.addSilentRenewError(() => login());
    const callback = config.redirect_path ?? "/auth/callback";
    (async () => {
      if (window.location.pathname === callback) {
        const u = await mgr.signinRedirectCallback();
        set(u);
        const st = u.state as ReturnState | undefined;
        const to = st?.returnTo && st.returnTo.startsWith("/") && !st.returnTo.startsWith("//") ? st.returnTo : "/";
        // Il router deve sapere del cambio di percorso: history.replaceState non lo avvisa.
        navigate(to, { replace: true });
        setStatus("authenticated");
        return;
      }
      // Dopo un ricaricamento il token in memoria non c'è più: con una sessione attiva
      // sull'identity provider il ritorno è immediato.
      login();
    })().catch((e: unknown) => {
      setError(e instanceof Error ? e.message : String(e));
      setStatus("error");
    });
  }, [mgr, config.redirect_path, login, navigate]);

  const value = useMemo<AuthContextValue>(
    () => ({
      status,
      error,
      name,
      token: () => userRef.current?.access_token,
      login,
      logout: () => {
        if (!mgr) return;
        void mgr.signoutRedirect().catch(() => mgr.removeUser().then(() => window.location.assign("/")));
      },
      expired: () => {
        if (mgr && status === "authenticated") login();
      },
    }),
    [status, error, name, login, mgr],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth fuori da AuthProvider");
  return ctx;
}
