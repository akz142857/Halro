import { useQueryClient } from "@tanstack/react-query";
import { api, clearSensitiveClientState } from "./api";
import { confirmNavigation, Link, navigate, setNavigationBlocked, usePathname } from "./navigation";
import { resetAppearance } from "./theme";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

const navigation = [
  ["/admin", "overview", "M3 3h18v18H3zM8 8h8M8 12h5M8 16h7"],
  ["/admin/providers", "providers", "M12 3v4M6.5 5.5l2.8 2.8M17.5 5.5l-2.8 2.8M5 12h4m6 0h4M8 17h8v4H8z"],
  ["/admin/deployments", "deployments", "M12 3 4 7v10l8 4 8-4V7zM4 7l8 4 8-4M12 11v10"],
  ["/admin/routes", "routes", "M5 5h6a4 4 0 0 1 4 4v10M11 19h8M16 16l3 3-3 3"],
  ["/admin/policies", "policies", "M12 3 5 6v5c0 4.8 2.9 8.2 7 10 4.1-1.8 7-5.2 7-10V6zM9 12l2 2 4-4"],
  ["/admin/projects", "projects", "M4 6h16v13H4zM8 6V4h8v2M8 11h8M8 15h5"],
  ["/admin/developer", "developer", "M8 8 4 12l4 4M16 8l4 4-4 4M14 5l-4 14"],
  ["/admin/usage", "usage", "M4 19V9M10 19V5M16 19v-7M22 19H2"],
  ["/admin/operations", "operations", "M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z"],
	["/admin/master-key", "masterKey", "M7 10V7a5 5 0 0 1 10 0v3M5 10h14v11H5zM12 14v3"],
  ["/admin/settings", "settings", "M4 6h6m4 0h6M10 3v6M4 12h10m4 0h2M14 9v6M4 18h3m4 0h9M7 15v6"],
] as const;

function NavIcon({ path }: { path: string }) {
  return <span className="nav-code" aria-hidden="true"><svg viewBox="0 0 24 24"><path d={path} /></svg></span>;
}

function LogoutIcon() {
  return <svg aria-hidden="true" viewBox="0 0 24 24"><path d="M10 5H5v14h5M14 8l4 4-4 4M8 12h10" /></svg>;
}

export function Layout({
  username,
  children,
  restricted = false,
}: {
  username: string;
  children: ReactNode;
  restricted?: boolean;
}) {
  const { t } = useTranslation();
  const path = usePathname();
  const queryClient = useQueryClient();
  const [loggingOut, setLoggingOut] = useState(false);
  const logout = async () => {
    if (loggingOut) return;
    if (!confirmNavigation()) return;
    setLoggingOut(true);
    try {
      await api.logout();
    } finally {
      clearSensitiveClientState();
      resetAppearance();
      queryClient.clear();
      setNavigationBlocked(false);
      navigate("/admin/login");
    }
  };
  return (
    <div className="shell">
      <a className="skip-link" href="#main-content">{t("navigation.skip")}</a>
      <aside className="sidebar">
        <Link href="/admin" className="brand" aria-label={`Heimdall ${t("navigation.overview")}`}>
          <span className="brand-mark">H</span>
          <span>
            <strong>HEIMDALL</strong>
            <small>{t("navigation.productSubtitle")}</small>
          </span>
        </Link>
        <nav aria-label={t("navigation.label")}>
          {navigation.filter(([href]) => !restricted || href === "/admin/settings").map(([href, key, icon]) => {
            const active = href === "/admin"
              ? path === href
              : path === href || path.startsWith(`${href}/`);
            return (
              <Link href={href} className={active ? "active" : ""} ariaCurrent={active ? "page" : undefined} key={href}>
                <NavIcon path={icon} />
                <span><strong>{t(`navigation.${key}`)}</strong></span>
              </Link>
            );
          })}
        </nav>
        <footer className="account-footer">
          <div className="account-card">
            <div className="operator">
              <span className="avatar">{username.slice(0, 1).toUpperCase()}</span>
              <span className="operator-copy"><small>{t("navigation.localAdmin")}</small><strong title={username}>{username}</strong></span>
            </div>
            <button className="logout-button" onClick={logout} disabled={loggingOut}>
              <span>{loggingOut ? t("navigation.loggingOut") : t("navigation.logout")}</span>
              <LogoutIcon />
            </button>
          </div>
        </footer>
      </aside>
      <main className="main" id="main-content" tabIndex={-1}>
        <div className="topline">
          <span><i className="pulse" /> {t("navigation.gatewayOnline")}</span>
          <span>{t("navigation.localControl")}</span>
        </div>
        {children}
      </main>
    </div>
  );
}
