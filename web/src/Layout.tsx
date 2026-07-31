import { useQueryClient } from "@tanstack/react-query";
import { api, clearSensitiveClientState } from "./api";
import { Link, navigate, usePathname } from "./navigation";
import type { ReactNode } from "react";

const navigation = [
  ["/admin", "Overview", "总览"],
  ["/admin/projects", "Projects", "项目与 Key"],
  ["/admin/providers", "Providers", "凭据与 Provider"],
  ["/admin/deployments", "Deployments", "模型部署"],
  ["/admin/routes", "Routes", "模型路由"],
  ["/admin/policies", "Policies", "Token Guard"],
  ["/admin/usage", "Usage", "用量"],
  ["/admin/operations", "Operations", "告警与审计"],
  ["/admin/settings", "Settings", "系统"],
] as const;

export function Layout({
  username,
  children,
}: {
  username: string;
  children: ReactNode;
}) {
  const path = usePathname();
  const queryClient = useQueryClient();
  const logout = async () => {
    try {
      await api.logout();
    } finally {
      clearSensitiveClientState();
      queryClient.clear();
      navigate("/admin/login");
    }
  };
  return (
    <div className="shell">
      <a className="skip-link" href="#main-content">跳到主要内容</a>
      <aside className="sidebar">
        <Link href="/admin" className="brand" aria-label="Heimdall 总览">
          <span className="brand-mark">H</span>
          <span>
            <strong>HEIMDALL</strong>
            <small>LLM CONTROL PLANE</small>
          </span>
        </Link>
        <nav aria-label="主导航">
          {navigation.map(([href, name, label]) => {
            const active = href === "/admin"
              ? path === href
              : path === href || path.startsWith(`${href}/`);
            return (
              <Link href={href} className={active ? "active" : ""} ariaCurrent={active ? "page" : undefined} key={href}>
                <span className="nav-code">{name.slice(0, 2).toUpperCase()}</span>
                <span>
                  <strong>{name}</strong>
                  <small>{label}</small>
                </span>
              </Link>
            );
          })}
        </nav>
        <footer>
          <div className="operator">
            <span className="avatar">{username.slice(0, 1).toUpperCase()}</span>
            <span><small>LOCAL ADMIN</small><strong>{username}</strong></span>
          </div>
          <button className="text-button" onClick={logout}>安全退出</button>
        </footer>
      </aside>
      <main className="main" id="main-content" tabIndex={-1}>
        <div className="topline">
          <span><i className="pulse" /> GATEWAY ONLINE</span>
          <span>LOCAL CONTROL / NO CLOUD DEPENDENCY</span>
        </div>
        {children}
      </main>
    </div>
  );
}
