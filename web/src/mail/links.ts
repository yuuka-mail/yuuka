// Links in mail are untrusted (§8.3): the reader is shown where a link
// really goes, next to whatever its text claims.

export interface Target {
  href: string;
  /** What to show of the target: the host of a web link, the address of mailto and tel. */
  shown: string;
  /** The text claims a host that is not this one. */
  mismatch: boolean;
}

/**
 * The target of a link, or undefined when it is not one the view keeps
 * (http, https, mailto, tel); such a span renders as plain text. The
 * server already drops other schemes; this is the second lock.
 */
export function linkTarget(href: string, text: string): Target | undefined {
  let u: URL;
  try {
    u = new URL(href);
  } catch {
    return undefined;
  }
  switch (u.protocol) {
    case "http:":
    case "https:": {
      // URL.hostname is punycode for international names, so a
      // look-alike host shows as the xn-- name it really is.
      const host = u.hostname;
      const claimed = claimedHost(text);
      const related = !claimed || claimed === host || host.endsWith("." + claimed) || claimed.endsWith("." + host);
      return { href: u.href, shown: host, mismatch: !related };
    }
    case "mailto:":
    case "tel:": {
      let shown = u.pathname;
      try {
        shown = decodeURIComponent(shown);
      } catch {
        // Keep it encoded.
      }
      return { href: u.href, shown, mismatch: false };
    }
  }
  return undefined;
}

/** The host a link's text claims, when the text looks like a URL or a bare domain. */
export function claimedHost(text: string): string | undefined {
  const m = /^\s*(?:https?:\/\/)?((?:[a-z0-9-]+\.)+[a-z]{2,})\.?(?:[:/?#]\S*)?\s*$/i.exec(text);
  return m?.[1]?.toLowerCase();
}
