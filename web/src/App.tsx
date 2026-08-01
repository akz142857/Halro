import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { api, ApiError } from "./api";
import { Layout } from "./Layout";
import { Login } from "./Login";
import { Setup } from "./Setup";
import { Loading } from "./components";
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

export function App() {
  const path = usePathname();
  const queryClient = useQueryClient();
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
    if (session.data && path === "/admin/login") navigate("/admin");
  }, [path, session.data]);
  if (setup.isPending) {
    return <div className="boot"><span className="brand-mark">H</span><Loading label="正在检查初始化状态" /></div>;
  }
  if (setup.isError) {
    return (
      <div className="boot">
        <span className="brand-mark">H</span>
        <div className="notice error" role="alert">无法读取初始化状态，请确认 Heimdall Admin 服务可用。</div>
        <button className="button primary" onClick={() => setup.refetch()}>重试</button>
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
    return <div className="boot"><span className="brand-mark">H</span><Loading label="正在验证本机会话" /></div>;
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
  return (
    <Layout username={session.data.username}>
      <Route path={path} />
    </Layout>
  );
}

function Route({ path }: { path: string }) {
  if (path === "/admin" || path === "/admin/") return <DashboardPage />;
  if (path.startsWith("/admin/projects")) return <ProjectsPage />;
  if (path.startsWith("/admin/providers")) return <ProvidersPage />;
  if (path.startsWith("/admin/deployments")) return <DeploymentsPage />;
  if (path.startsWith("/admin/routes")) return <RoutesPage />;
  if (path.startsWith("/admin/policies") || path.startsWith("/admin/token-guard")) return <PoliciesPage />;
  if (path.startsWith("/admin/usage")) return <UsagePage />;
  if (path.startsWith("/admin/operations") || path.startsWith("/admin/audit") || path.startsWith("/admin/alerts")) {
    return <OperationsPage />;
  }
  if (path.startsWith("/admin/settings")) return <SettingsPage />;
  return (
    <section className="not-found">
      <p className="eyebrow">ROUTE NOT FOUND</p>
      <h1>这个控制台页面不存在。</h1>
      <button className="button primary" onClick={() => navigate("/admin")}>返回总览</button>
    </section>
  );
}
