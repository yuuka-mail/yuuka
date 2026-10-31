import { useSyncExternalStore } from "react";

// A router is four functions here: the WebUI has five screens and every
// view state lives in the query string, so a routing library would be
// more code to keep up with than this.

const changed = "yuuka:navigate";

function subscribe(notify: () => void) {
  window.addEventListener("popstate", notify);
  window.addEventListener(changed, notify);
  return () => {
    window.removeEventListener("popstate", notify);
    window.removeEventListener(changed, notify);
  };
}

function snapshot() {
  return window.location.pathname + window.location.search + window.location.hash;
}

/** The current location; re-renders on navigation. */
export function useLocation(): URL {
  const href = useSyncExternalStore(subscribe, snapshot);
  return new URL(href, window.location.origin);
}

/** Navigates within the app without a page load. */
export function navigate(to: string, { replace = false } = {}) {
  if (to === snapshot()) return;
  if (replace) window.history.replaceState(null, "", to);
  else window.history.pushState(null, "", to);
  window.dispatchEvent(new Event(changed));
}
