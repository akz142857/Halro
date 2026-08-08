import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import { api, ApiError } from "./api";
import { Layout } from "./Layout";
import { Login } from "./Login";
import { Setup } from "./Setup";
import { ErrorBoundary, Loading } from "./components";
import { navigate, usePathname } from "./navigation";
import { DashboardPage } from "./pages/DashboardPage";
import { DeploymentsPage } from "./pages/DeploymentsPage";
import { OperationsPage } from "./pages/OperationsPage";
import { PoliciesPage } from "./pages/PoliciesPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { ProvidersPage } from "./pages/ProvidersPage";
import { RoutesPage } from "./pages/RoutesPage";
import { SettingsPage } from "./pages/SettingsPage";
import { UsagePage } from "./pages/UsagePage";
import { MasterKeyCustodyPage } from "./pages/MasterKeyCustodyPage";
import { DeveloperPage } from "./pages/DeveloperPage";
import { applyLocale, applyPreference, resolveLocale } from "./i18n";
import { applyAppearance, normalizeAppearance, resetAppearance } from "./theme";
import { adoptTimeContext, resetAccountingTimeZone } from "./timezone";

export function App() {
  const { t } = useTranslation();
  const path = usePathname();
  const queryClient = useQueryClient();
  const uiBootstrap = useQuery({
    queryKey: ["ui-bootstrap"],
    queryFn: api.uiBootstrap,
    staleTime: Infinity,
  });
  const setup = useQuery({
    queryKey: ["setup"],
    queryFn: api.setupStatus,
    retry: 2,
    staleTime: 10_000,
  });
  const session = useQuery({
    queryKey: ["session"],
    queryFn: api.session,
    retry: (count, error) => !(error instanceof ApiError && error.status === 401) && count < 2,
    staleTime: 60_000,
    enabled: setup.data?.setup_required === false,
  });
  useEffect(() => {
    if (uiBootstrap.data) {
      void applyLocale(resolveLocale(undefined, uiBootstrap.data.default_locale));
    }
  }, [uiBootstrap.data]);
  useEffect(() => {
    if (session.data) {
      void applyPreference(session.data.locale, uiBootstrap.data?.default_locale);
    }
  }, [session.data, uiBootstrap.data?.default_locale]);
  useEffect(() => {
    // Authenticated: apply the admin's server-confirmed appearance. Otherwise
    // (login, setup, unauthenticated errors) fall back to the Dark default so
    // no previous admin's preference leaks before authentication (PRD §4.3).
    if (session.data) {
      applyAppearance(normalizeAppearance(session.data.appearance));
    } else {
      resetAppearance();
    }
  }, [session.data]);
  // Every timestamp in the console renders in the server's accounting zone, so
  // it has to be known before any page draws — not just on the pages that
  // happen to fetch a dashboard. Signing out returns to the UTC default rather
  // than leaving the previous instance's zone on screen.
  const accounting = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    enabled: Boolean(session.data),
    staleTime: 60_000,
  });
  useEffect(() => {
    if (session.data) adoptTimeContext(accounting.data?.time_context);
    else resetAccountingTimeZone();
  }, [session.data, accounting.data]);
  useEffect(() => {
    if (session.data && path === "/admin/login") navigate(session.data.mfa_setup_required ? "/admin/settings" : "/admin");
  }, [path, session.data]);
  useEffect(() => {
    if (session.data && path !== "/admin/login") document.getElementById("main-content")?.focus();
  }, [path, session.data]);
  if (setup.isPending) {
    return <div className="boot"><span className="brand-mark">H</span><Loading label={t("app.checkingSetup")} /></div>;
  }
  if (setup.isError) {
    return (
      <div className="boot">
        <span className="brand-mark">H</span>
        <div className="notice error" role="alert">{t("app.setupUnavailable")}</div>
        <button className="button primary" onClick={() => setup.refetch()}>{t("common.retry")}</button>
      </div>
    );
  }
  if (setup.data.setup_required) {
    return (
      <Setup
        tokenRequired={setup.data.token_required}
        onAlreadyComplete={() => {
          queryClient.setQueryData(["setup"], { ...setup.data, setup_required: false });
          queryClient.removeQueries({ queryKey: ["session"] });
          navigate("/admin/login");
        }}
        onSuccess={(created) => {
          queryClient.setQueryData(["setup"], { ...setup.data, setup_required: false });
          queryClient.setQueryData(["session"], created);
          navigate("/admin");
        }}
      />
    );
  }
  if (session.isPending) {
    return <div className="boot"><span className="brand-mark">H</span><Loading label={t("app.checkingSession")} /></div>;
  }
  if (session.isError) {
    return (
      <Login
        onSuccess={() => {
          queryClient.invalidateQueries({ queryKey: ["session"] });
          navigate("/admin");
        }}
      />
    );
  }
  if (session.data.mfa_setup_required) {
    return <Layout username={session.data.username} restricted><SettingsPage mfaSetupRequired /></Layout>;
  }
  // Keyed by path so navigating away from a crashed page recovers on its own.
  return <Layout username={session.data.username}><ErrorBoundary key={path}><Route path={path} /></ErrorBoundary></Layout>;
}

function Route({ path }: { path: string }) {
  const { t } = useTranslation();
  if (path === "/admin" || path === "/admin/") return <DashboardPage />;
  if (path.startsWith("/admin/projects")) return <ProjectsPage />;
  if (path.startsWith("/admin/developer")) return <DeveloperPage />;
  if (path.startsWith("/admin/providers")) return <ProvidersPage />;
  if (path.startsWith("/admin/deployments")) return <DeploymentsPage />;
  if (path.startsWith("/admin/routes")) return <RoutesPage />;
  if (path.startsWith("/admin/policies") || path.startsWith("/admin/token-guard")) return <PoliciesPage />;
  if (path.startsWith("/admin/usage")) return <UsagePage />;
  if (path.startsWith("/admin/operations") || path.startsWith("/admin/audit") || path.startsWith("/admin/alerts")) {
    return <OperationsPage />;
  }
  if (path.startsWith("/admin/settings")) return <SettingsPage />;
	if (path.startsWith("/admin/master-key")) return <MasterKeyCustodyPage />;
  return (
    <section className="not-found">
      <p className="eyebrow">{t("app.notFoundEyebrow")}</p>
      <h1>{t("app.notFound")}</h1>
      <button className="button primary" onClick={() => navigate("/admin")}>{t("app.backOverview")}</button>
    </section>
  );
}
