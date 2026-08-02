import { useEffect, useState, type MouseEvent, type ReactNode } from "react";

const navigationEvent = "heimdall:navigate";
let navigationBlocked = false;
let blockedPath = "";

export function setNavigationBlocked(blocked: boolean) {
  navigationBlocked = blocked;
  blockedPath = blocked ? window.location.pathname : "";
}

export function navigate(path: string) {
  if (navigationBlocked) return;
  if (window.location.pathname === path) return;
  window.history.pushState({}, "", path);
  window.dispatchEvent(new Event(navigationEvent));
}

export function usePathname() {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const update = () => {
      if (navigationBlocked && blockedPath && window.location.pathname !== blockedPath) {
        window.history.replaceState({}, "", blockedPath);
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
