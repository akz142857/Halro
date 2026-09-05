import { useEffect, useState, type MouseEvent, type ReactNode } from "react";

const navigationEvent = "halro:navigate";
let navigationBlocked = false;
let blockedPath = "";
let blockedMessage = "";

export function setNavigationBlocked(blocked: boolean, message = "") {
  navigationBlocked = blocked;
  blockedPath = blocked ? window.location.pathname + window.location.search : "";
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
  // Compared against the query too, not the path alone. Several destinations carry
  // their subject in the query — a request ID, a project — so comparing only the
  // path made "go to this page filtered differently" a no-op whenever the reader
  // was already on that page, including clearing a filter back to the plain list.
  if (window.location.pathname + window.location.search === path) return;
  window.history.pushState({}, "", path);
  window.dispatchEvent(new Event(navigationEvent));
}

export function useNavigationLocation() {
  const current = () => window.location.pathname + window.location.search;
  const [location, setLocation] = useState(current);
  useEffect(() => {
    const update = () => {
      if (navigationBlocked && blockedPath && current() !== blockedPath) {
        if (confirmNavigation()) {
          setLocation(current());
          return;
        }
        window.history.pushState({}, "", blockedPath);
        setLocation(blockedPath);
        return;
      }
      setLocation(current());
    };
    window.addEventListener("popstate", update);
    window.addEventListener(navigationEvent, update);
    return () => {
      window.removeEventListener("popstate", update);
      window.removeEventListener(navigationEvent, update);
    };
  }, []);
  return location;
}

export function usePathname() {
  return useNavigationLocation().split("?", 1)[0];
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
