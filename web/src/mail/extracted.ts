import type { Extracted, MessageSummary } from "@/api/types";

import { linkTarget } from "./links";

// What the notification zone shows in place of a subject (§12.9): the
// code the extractor found, or failing that the sign-in link, and how
// long it lasts. Both are yuuka's assertions about the sender's text.

/** Whether a row shows its extraction: notifications only, wherever they are listed. */
export function shownExtraction(m: MessageSummary): Extracted | undefined {
  if (m.class !== "machine" || !m.extracted) return undefined;
  return m.extracted;
}

export interface Left {
  text: string;
  expired: boolean;
}

/** How long is left until expiresAt at now: "9 min left", "expired". */
export function timeLeft(expiresAt: string, now: number): Left {
  const s = Math.floor((Date.parse(expiresAt) - now) / 1000);
  if (Number.isNaN(s) || s <= 0) return { text: "expired", expired: true };
  if (s < 60) return { text: `${s} s left`, expired: false };
  const m = Math.ceil(s / 60);
  if (m < 120) return { text: `${m} min left`, expired: false };
  const h = Math.floor(m / 60);
  if (h < 48) return { text: `${h} h left`, expired: false };
  return { text: `${Math.floor(h / 24)} d left`, expired: false };
}

/**
 * The link to offer as "Open host": only http and https, and the host the
 * browser will really go to (punycode as is, §12.7). Undefined when the
 * value is not such a link; then nothing is offered.
 */
export function openable(url: string): { href: string; host: string } | undefined {
  const t = linkTarget(url, "");
  if (!t || !t.href.startsWith("http")) return undefined;
  return { href: t.href, host: t.shown };
}
