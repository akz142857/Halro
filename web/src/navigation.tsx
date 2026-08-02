import { useEffect, useState, type MouseEvent, type ReactNode } from "react";

const navigationEvent = "heimdall:navigate";
let navigationBlocked = false;
let blockedPath = "";
let blockedMessage = "";

export function setNavigationBlocked(blocked: boolean, message = "") {
  navigationBlocked = blocked;
  blockedPath = blocked ? window.location.pathname : "";
  blockedMessage = blocked ? message : "";
}

export function confirmNavigation() {
  if (!navigationBlocked) return true;
  if (!window.confirm(blockedMessage)) return false;
  setNavigationBlocked(false);
  return true;
}

export function navigate(path: string) {
  if (!confirmNavigation()) return;
  if (window.location.pathname === path) return;
  window.history.pushState({}, "", path);
  window.dispatchEvent(new Event(navigationEvent));
}

export function usePathname() {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const update = () => {
      if (navigationBlocked && blockedPath && window.location.pathname !== blockedPath) {
        if (confirmNavigation()) {
          setPath(window.location.pathname);
          return;
        }
        window.history.pushState({}, "", blockedPath);
        setPath(blockedPath);
        return;
      }
      setPath(window.location.pathname);
    };
    window.addEventListener("popstate", update);
    window.addEventListener(navigationEvent, update);
    return () => {
      window.removeEventListener("popstate", update);
      window.removeEventListener(navigationEvent, update);
    };
  }, []);
  return path;
}

export function Link({
  href,
  children,
  className,
  ariaCurrent,
}: {
  href: string;
  children: ReactNode;
  className?: string;
  ariaCurrent?: "page";
}) {
  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) return;
    event.preventDefault();
    navigate(href);
  };
  return <a href={href} onClick={onClick} className={className} aria-current={ariaCurrent}>{children}</a>;
}
