import { useQuery } from "@tanstack/react-query";
import { BarChart3, CalendarClock, ChevronDown, LayoutGrid, ListChecks, LogOut, Moon, Rocket, ShieldAlert, Sun } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { NavLink, useLocation } from "react-router-dom";

import { unwrap, type Status } from "@/api/client";
import { useApi, useMe, useUIConfig } from "@/api/context";
import { useAuth } from "@/auth/auth";
import { useI18n, type Lang } from "@/i18n/i18n";
import { canSomewhere } from "@/lib/permissions";
import { useTheme } from "@/lib/theme";

export function Logo({ className = "h-7 w-7" }: { className?: string }) {
  return (
    <svg viewBox="0 0 64 64" className={className} aria-hidden focusable="false">
      <defs>
        <linearGradient id="logo-g" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="rgb(var(--accent))" />
          <stop offset="1" stopColor="rgb(var(--violet))" />
        </linearGradient>
      </defs>
      <path d="M32 4 56 18v28L32 60 8 46V18Z" fill="none" stroke="url(#logo-g)" strokeWidth="4" strokeLinejoin="round" />
      <path d="M20 42V26l6 6 6-6 6 6 6-6v16" fill="none" stroke="url(#logo-g)" strokeWidth="4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function useApprovalsCount(enabled: boolean) {
  const api = useApi();
  return useQuery({
    queryKey: ["approvals"],
    queryFn: async (): Promise<Status[]> => unwrap(await api.GET("/api/v1/approvals")).approvals,
    enabled,
    refetchInterval: 30_000,
  });
}

function UserMenu() {
  const { t } = useI18n();
  const auth = useAuth();
  const me = useMe();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === "Escape" : !ref.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", close);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", close);
    };
  }, [open]);
  const id = me.data;
  const name = auth.name ?? id?.name ?? t("user.anonymous");
  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        className="flex h-9 items-center gap-2 rounded-lg px-2 text-sm text-muted hover:bg-raised/70 hover:text-fg"
        aria-expanded={open}
        aria-controls="user-menu"
        onClick={() => setOpen((o) => !o)}
      >
        <span className="flex h-6 w-6 items-center justify-center rounded-full bg-accent/15 font-mono text-xs uppercase text-accent" aria-hidden>
          {name.slice(0, 1)}
        </span>
        <span className="max-w-[10rem] truncate">{name}</span>
        <ChevronDown className="h-4 w-4" aria-hidden />
        <span className="sr-only">{t("user.menu")}</span>
      </button>
      {open && (
        <div id="user-menu" className="card absolute right-0 z-30 mt-2 w-72 bg-surface p-4 text-sm shadow-glow">
          <p className="font-medium">{name}</p>
          {id?.subject && <p className="font-mono text-xs text-muted">{id.subject}</p>}
          {id?.groups && id.groups.length > 0 && (
            <p className="mt-3">
              <span className="text-muted">{t("user.groups")}: </span>
              {id.groups.join(", ")}
            </p>
          )}
          {id?.auth_enabled && id.grants.length > 0 && (
            <div className="mt-2">
              <p className="text-muted">{t("user.roles")}:</p>
              <ul className="mt-1 space-y-1">
                {id.grants.map((g, i) => (
                  <li key={i} className="font-mono text-xs">
                    {g.role} · {g.applications.join(",")} / {g.environments.join(",")}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {auth.status === "authenticated" && (
            <button
              type="button"
              onClick={auth.logout}
              className="mt-4 flex w-full items-center gap-2 rounded-lg border border-border px-3 py-2 hover:bg-raised"
            >
              <LogOut className="h-4 w-4" aria-hidden />
              {t("user.signOut")}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export function Layout({ children }: { children: ReactNode }) {
  const { t, lang, setLang } = useI18n();
  const { theme, toggle } = useTheme();
  const config = useUIConfig();
  const me = useMe();
  const canApprove = canSomewhere(me.data, "deployment.approve");
  const approvals = useApprovalsCount(canApprove);
  const waiting = approvals.data?.length ?? 0;
  const location = useLocation();
  const previous = useRef<string>();
  const callback = config.redirect_path ?? "/auth/callback";

  // Dopo un cambio di pagina il focus va al titolo, così gli screen reader annunciano la nuova
  // vista. Non al primo caricamento né al ritorno dal login, che sono caricamenti di pagina.
  useEffect(() => {
    const from = previous.current;
    previous.current = location.pathname;
    if (from === undefined || from === callback) return;
    // Le pagine caricate in differita (Metriche) mostrano il titolo un po' dopo: lo si attende.
    const focus = () => {
      const h = document.getElementById("page-title");
      h?.focus();
      return Boolean(h);
    };
    if (focus()) return;
    const main = document.getElementById("main");
    if (!main) return;
    const obs = new MutationObserver(() => {
      if (focus()) obs.disconnect();
    });
    obs.observe(main, { childList: true, subtree: true });
    const stop = setTimeout(() => obs.disconnect(), 5000);
    return () => {
      obs.disconnect();
      clearTimeout(stop);
    };
  }, [location.pathname, callback]);

  const nav = [
    { to: "/deployments", label: t("nav.deployments"), icon: Rocket },
    { to: "/approvals", label: t("nav.approvals"), icon: ListChecks, count: canApprove ? waiting : 0 },
    { to: "/inventory", label: t("nav.inventory"), icon: LayoutGrid },
    { to: "/calendar", label: t("nav.calendar"), icon: CalendarClock },
    { to: "/metrics", label: t("nav.metrics"), icon: BarChart3 },
  ];

  return (
    <div className="flex min-h-screen flex-col">
      <a href="#main" className="skip-link">
        {t("app.skip")}
      </a>
      <header className="sticky top-0 z-20 border-b border-border/60 bg-bg/80 backdrop-blur-md">
        <div className="mx-auto flex max-w-7xl flex-wrap items-center gap-x-6 gap-y-2 px-4 py-2 sm:px-6">
          <NavLink to="/deployments" className="flex items-center gap-2 rounded-lg font-display text-lg font-semibold tracking-wide">
            <Logo />
            <span>manent</span>
          </NavLink>
          <nav aria-label={t("nav.main")} className="order-last w-full overflow-x-auto md:order-none md:w-auto">
            <ul className="flex gap-1">
              {nav.map(({ to, label, icon: Icon, count }) => (
                <li key={to}>
                  <NavLink
                    to={to}
                    className={({ isActive }) =>
                      `flex h-9 items-center gap-2 whitespace-nowrap rounded-lg px-3 text-sm transition-colors ${
                        isActive ? "bg-accent/10 font-medium text-accent" : "text-muted hover:bg-raised/70 hover:text-fg"
                      }`
                    }
                  >
                    <Icon className="h-4 w-4" aria-hidden />
                    {label}
                    {count ? (
                      <span className="rounded-full bg-warn/20 px-1.5 text-xs font-semibold text-warn">
                        {count}
                        <span className="sr-only"> — {t("nav.approvalsWaiting", { count })}</span>
                      </span>
                    ) : null}
                  </NavLink>
                </li>
              ))}
            </ul>
          </nav>
          <div className="ml-auto flex items-center gap-1">
            <label className="sr-only" htmlFor="lang-select">
              {t("lang.label")}
            </label>
            <select
              id="lang-select"
              value={lang}
              onChange={(e) => setLang(e.target.value as Lang)}
              className="h-9 rounded-lg border border-transparent bg-transparent px-2 text-sm text-muted hover:border-border hover:text-fg"
            >
              <option value="en">EN</option>
              <option value="it">IT</option>
            </select>
            <button
              type="button"
              onClick={toggle}
              className="flex h-9 w-9 items-center justify-center rounded-lg text-muted hover:bg-raised/70 hover:text-fg"
              aria-label={theme === "dark" ? t("theme.toLight") : t("theme.toDark")}
              title={theme === "dark" ? t("theme.toLight") : t("theme.toDark")}
            >
              {theme === "dark" ? <Sun className="h-4 w-4" aria-hidden /> : <Moon className="h-4 w-4" aria-hidden />}
            </button>
            <UserMenu />
          </div>
        </div>
        <div className="gradient-line h-px opacity-50" aria-hidden />
      </header>
      {config.auth_mode === "none" && (
        <div role="note" className="border-b border-warn/40 bg-warn/10">
          <p className="mx-auto flex max-w-7xl items-center gap-2 px-4 py-2 text-sm text-warn sm:px-6">
            <ShieldAlert className="h-4 w-4 shrink-0" aria-hidden />
            {t("app.authDisabled")}
          </p>
        </div>
      )}
      <main id="main" tabIndex={-1} className="mx-auto w-full max-w-7xl flex-1 px-4 py-6 outline-none sm:px-6 sm:py-8">
        {children}
      </main>
      <footer className="border-t border-border/60">
        <p className="mx-auto max-w-7xl px-4 py-4 font-mono text-xs text-muted sm:px-6">{t("app.version", { version: config.version })}</p>
      </footer>
    </div>
  );
}
