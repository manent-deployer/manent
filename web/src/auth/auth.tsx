import { InMemoryWebStorage, UserManager, WebStorageStateStore, type User } from "oidc-client-ts";
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router";

import type { UIConfig } from "@/lib/config";
import { safeReturnPath } from "@/lib/returnPath";

// Login state. With auth_mode "none" there is no login; "unavailable" is a server with
// authentication enabled but no OIDC client for the UI.
export type AuthStatus = "loading" | "anonymous" | "authenticated" | "redirecting" | "unavailable" | "insecure" | "error";

interface AuthContextValue {
  status: AuthStatus;
  error?: string;
  /** Token to send to the API; undefined without authentication. */
  token: () => string | undefined;
  /** Goes to the identity provider and comes back to the current page. */
  login: () => void;
  logout: () => void;
  /** Called by the API client on a 401: the token is no longer valid. */
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
    // Tokens stay only in memory: no localStorage or cookies, so no CSRF and no token readable by
    // other tabs. The redirect state (PKCE) lives in sessionStorage for the login only.
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
    // PKCE uses crypto.subtle, available only in a secure context (HTTPS or localhost).
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

  // Starting the login (callback or redirect) must run only once: the effect can restart when
  // the path changes (navigate) or, in development, because of StrictMode.
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
        const to = safeReturnPath(st?.returnTo);
        // The router must know about the path change: history.replaceState does not notify it.
        navigate(to, { replace: true });
        setStatus("authenticated");
        return;
      }
      // After a reload the in-memory token is gone: with an active session on the identity
      // provider the return is immediate.
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
