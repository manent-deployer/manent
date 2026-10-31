import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AlertTriangle, KeyRound } from "lucide-react";
import { lazy, Suspense, useEffect, useState, type ReactNode } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { ApiError, type Api } from "@/api/client";
import { ApiProvider, useUIConfig } from "@/api/context";
import { AuthProvider, useAuth } from "@/auth/auth";
import { Layout, Logo } from "@/components/Layout";
import { ToastProvider } from "@/components/Toaster";
import { Button, Loading, PageHeader } from "@/components/ui";
import { I18nProvider, useI18n, type Lang } from "@/i18n/i18n";
import { loadConfig, type UIConfig } from "@/lib/config";
import { ThemeProvider } from "@/lib/theme";
import { ActivityPage } from "@/pages/Activity";
import { ApprovalsPage } from "@/pages/Approvals";
import { CalendarPage } from "@/pages/Calendar";
import { DeploymentDetailPage } from "@/pages/DeploymentDetail";
import { DeploymentsPage } from "@/pages/Deployments";
import { InventoryPage } from "@/pages/Inventory";
import { PlatformPage } from "@/pages/Platform";
import { PoliciesPage } from "@/pages/Policies";
import { ReleaseDetailPage, ReleasesPage } from "@/pages/Releases";
import { TenantDetailPage, TenantsPage } from "@/pages/Tenants";

// Metrics use Recharts (about 100 KB compressed): loaded only when the page opens.
const MetricsPage = lazy(() => import("@/pages/Metrics").then((m) => ({ default: m.MetricsPage })));

export function newQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // A 4xx does not improve by retrying; other errors are retried twice.
        retry: (n, err) => !(err instanceof ApiError && err.status >= 400 && err.status < 500) && n < 2,
        refetchOnWindowFocus: true,
        staleTime: 5_000,
      },
    },
  });
}

function Centered({ children }: { children: ReactNode }) {
  return (
    <main id="main" className="mx-auto flex min-h-screen max-w-lg flex-col items-center justify-center gap-4 px-4 text-center">
      <Logo className="h-12 w-12" />
      {children}
    </main>
  );
}

function NotFound() {
  const { t } = useI18n();
  return <PageHeader title={t("notFound.title")} lead={t("notFound.body")} />;
}

export function AppRoutes() {
  const config = useUIConfig();
  return (
    <Layout>
      <Routes>
        {/* Back from login: AuthProvider completes the sign-in and goes to the requested page. */}
        <Route path={config.redirect_path ?? "/auth/callback"} element={<Loading />} />
        <Route path="/" element={<Navigate to="/deployments" replace />} />
        <Route path="/deployments" element={<DeploymentsPage />} />
        <Route path="/deployments/:id" element={<DeploymentDetailPage />} />
        <Route path="/activity" element={<ActivityPage />} />
        <Route path="/approvals" element={<ApprovalsPage />} />
        <Route path="/inventory" element={<InventoryPage />} />
        <Route path="/releases" element={<ReleasesPage />} />
        <Route path="/releases/:id" element={<ReleaseDetailPage />} />
        <Route path="/calendar" element={<CalendarPage />} />
        <Route path="/policies" element={<PoliciesPage />} />
        <Route path="/tenants" element={<TenantsPage />} />
        <Route path="/tenants/:name" element={<TenantDetailPage />} />
        <Route path="/platform" element={<PlatformPage />} />
        <Route
          path="/metrics"
          element={
            <Suspense fallback={<Loading />}>
              <MetricsPage />
            </Suspense>
          }
        />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </Layout>
  );
}

/** Gate shows the app only once login is done; otherwise the login state. */
function Gate({ children }: { children: ReactNode }) {
  const auth = useAuth();
  const { t } = useI18n();
  switch (auth.status) {
    case "authenticated":
    case "anonymous":
      return <>{children}</>;
    case "unavailable":
      return (
        <Centered>
          <KeyRound className="h-6 w-6 text-warn" aria-hidden />
          <h1 className="text-2xl font-semibold">{t("app.unavailable.title")}</h1>
          <p className="text-muted">{t("app.unavailable.body")}</p>
        </Centered>
      );
    case "insecure":
      return (
        <Centered>
          <KeyRound className="h-6 w-6 text-warn" aria-hidden />
          <h1 className="text-2xl font-semibold">{t("app.insecure.title")}</h1>
          <p className="text-muted">{t("app.insecure.body")}</p>
        </Centered>
      );
    case "error":
      return (
        <Centered>
          <AlertTriangle className="h-6 w-6 text-bad" aria-hidden />
          <h1 className="text-2xl font-semibold">{t("app.error.title")}</h1>
          <p className="break-words text-muted">{auth.error}</p>
          <Button variant="primary" onClick={auth.login}>
            {t("app.retry")}
          </Button>
        </Centered>
      );
    default:
      return (
        <Centered>
          <Loading label={t("app.redirecting")} />
        </Centered>
      );
  }
}

/** App with an already read configuration: used by tests with a fake API. */
export function AppWithConfig({ config, api, queryClient, lang }: { config: UIConfig; api?: Api; queryClient?: QueryClient; lang?: Lang }) {
  const [qc] = useState(() => queryClient ?? newQueryClient());
  return (
    <I18nProvider lang={lang}>
      <ThemeProvider>
        <AuthProvider config={config}>
          <QueryClientProvider client={qc}>
            <ApiProvider config={config} api={api}>
              <ToastProvider>
                <Gate>
                  <AppRoutes />
                </Gate>
              </ToastProvider>
            </ApiProvider>
          </QueryClientProvider>
        </AuthProvider>
      </ThemeProvider>
    </I18nProvider>
  );
}

export function App() {
  const [config, setConfig] = useState<UIConfig>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    loadConfig()
      .then(setConfig)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);
  if (error) {
    return (
      <I18nProvider>
        <Centered>
          <AlertTriangle className="h-6 w-6 text-bad" aria-hidden />
          <p role="alert" className="text-muted">
            {error}
          </p>
        </Centered>
      </I18nProvider>
    );
  }
  if (!config) {
    return (
      <I18nProvider>
        <Centered>
          <Loading />
        </Centered>
      </I18nProvider>
    );
  }
  return (
    <BrowserRouter>
      <AppWithConfig config={config} />
    </BrowserRouter>
  );
}
