import type { AccountBody, ManagedSource, SourceFolder, SourceState } from "@/api/types";
import { m } from "@/paraglide/messages.js";

// What the external mailbox settings decide before they ask (§9.5,
// §13.10). The server decides again (manage.SourceWidens and the rest);
// these say what a change will cost, so the form can say it before the
// passkey prompt, and how far a folder got in words. Pure, so tested.

const MiB = 1 << 20;

/** The Gmail preset's server (manage.GmailHost). */
export const gmailHost = "imap.gmail.com";

/** How far back the first sync goes: as set (an edit), everything, nothing before now, or n days. */
export type Backfill = "keep" | "all" | "none" | "days";

/** A pull account's form as typed. Sizes are MiB; blank takes the default. */
export interface AccountForm {
  name: string;
  preset: "gmail" | "";
  address: string;
  host: string;
  port: string;
  username: string;
  password: string;
  authservId: string;
  /** Comma-separated folders; blank for the default. */
  mailboxes: string;
  backfill: Backfill;
  days: string;
  maxMiB: string;
  /** Blank: the preset's budget; 0: no limit. */
  budgetMiB: string;
  poll: string;
}

const mib = (n: number) => String(Math.round((n / MiB) * 100) / 100);

/** The form of source s, or an empty Gmail one for a new account. Never a password. */
export function accountForm(s?: ManagedSource, name = ""): AccountForm {
  if (!s)
    return {
      name: "",
      preset: "gmail",
      address: "",
      host: "",
      port: "993",
      username: "",
      password: "",
      authservId: "",
      mailboxes: "",
      backfill: "all",
      days: "30",
      maxMiB: "",
      budgetMiB: "",
      poll: "60",
    };
  return {
    name,
    preset: s.preset ?? "",
    address: s.address,
    host: s.host,
    port: String(s.port),
    username: s.username,
    password: "",
    authservId: s.trusted_authserv_id ?? "",
    mailboxes: (s.mailboxes ?? []).join(", "),
    backfill: "keep",
    days: "30",
    maxMiB: mib(s.max_message_bytes),
    budgetMiB: mib(s.daily_byte_budget),
    poll: String(s.poll_seconds),
  };
}

const list = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

const count = (s: string, min: number, max = Number.MAX_SAFE_INTEGER) => {
  const n = Number(s);
  return s.trim() !== "" && Number.isFinite(n) && n >= min && n <= max;
};

/** Whether the server will take the form's shape; what it says of the values it says itself. */
export function validAccountForm(f: AccountForm, isNew: boolean): boolean {
  return (
    (!isNew || /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(f.name)) &&
    f.address.includes("@") &&
    (f.preset === "gmail" || f.host.trim() !== "") &&
    count(f.port, 1, 65535) &&
    Number.isInteger(Number(f.port)) &&
    (!isNew || f.password !== "") &&
    (f.backfill !== "days" || count(f.days, 1, 36600)) &&
    (f.maxMiB.trim() === "" || count(f.maxMiB, 0)) &&
    (f.budgetMiB.trim() === "" || count(f.budgetMiB, 0)) &&
    (f.poll.trim() === "" || count(f.poll, 1))
  );
}

/** The body of PUT /api/v1/admin/accounts/{name} for f over cur (absent: a new account). */
export function accountBody(
  f: AccountForm,
  cur: ManagedSource | undefined,
  now: Date,
): AccountBody {
  const b: AccountBody = { revision: cur?.revision ?? 0, address: f.address.trim() };
  if (f.preset) b.preset = f.preset;
  if (f.host.trim()) b.host = f.host.trim();
  if (f.port.trim()) b.port = Number(f.port);
  if (f.username.trim()) b.username = f.username.trim();
  if (f.password) b.password = f.password;
  if (f.authservId.trim()) b.trusted_authserv_id = f.authservId.trim();
  const boxes = list(f.mailboxes);
  if (boxes.length) b.mailboxes = boxes;
  switch (f.backfill) {
    case "keep":
      if (cur?.backfill_since) b.backfill_since = cur.backfill_since;
      break;
    case "none":
      b.backfill_since = now.toISOString();
      break;
    case "days":
      b.backfill_since = new Date(now.getTime() - Number(f.days) * 86_400_000).toISOString();
      break;
  }
  if (f.maxMiB.trim()) b.max_message_bytes = Math.round(Number(f.maxMiB) * MiB);
  if (f.budgetMiB.trim()) b.daily_byte_budget = Math.round(Number(f.budgetMiB) * MiB);
  if (f.poll.trim()) b.poll_seconds = Number(f.poll);
  return b;
}

/**
 * Whether writing b over cur lets more in or sends the password
 * somewhere new, and so asks for the passkey: mirrors
 * manage.SourceWidens for what the form can know. Blank fields take the
 * server's defaults, which it compares; when unsure this says yes, and
 * the cost is only a passkey prompt the server would not have asked for.
 */
export function sourceWidens(cur: ManagedSource | undefined, b: AccountBody): boolean {
  if (!cur || b.password) return true;
  const host = b.host?.toLowerCase() ?? (b.preset === "gmail" ? gmailHost : "");
  const user = b.username ?? b.address.toLowerCase();
  if (host !== cur.host || (b.port ?? 993) !== cur.port || user !== cur.username) return true;
  if (b.address !== cur.address || (b.preset ?? "") !== (cur.preset ?? "")) return true;
  if ((b.trusted_authserv_id ?? "") !== (cur.trusted_authserv_id ?? "")) return true;
  const was = cur.mailboxes,
    now = b.mailboxes;
  if (!was !== !now || (now ?? []).some((x) => !(was ?? []).includes(x))) return true;
  if (cur.backfill_since && (!b.backfill_since || b.backfill_since < cur.backfill_since))
    return true;
  if (b.max_message_bytes === undefined || b.max_message_bytes > cur.max_message_bytes) return true;
  if (
    cur.daily_byte_budget !== 0 &&
    (b.daily_byte_budget === undefined ||
      b.daily_byte_budget === 0 ||
      b.daily_byte_budget > cur.daily_byte_budget)
  )
    return true;
  return (b.poll_seconds ?? 60) < cur.poll_seconds;
}

/** Where a source connects, as its password's audience: user@host:port. */
export function sourceDestination(s: { username: string; host: string; port: number }): string {
  const host = s.host.includes(":") ? `[${s.host}]` : s.host;
  return `${s.username}@${host}:${s.port}`;
}

/** Bytes in the largest binary unit that keeps them short: "32 MiB", "2 GiB". */
export function bytes(n: number): string {
  // Sizes reach past 2^31, where JavaScript's shifts stop: powers, not <<.
  if (n >= 1024 ** 3 && n % (1024 ** 3 / 4) === 0) return `${n / 1024 ** 3} GiB`;
  if (n >= MiB) return `${Math.round((n / MiB) * 10) / 10} MiB`;
  if (n >= 1024) return `${Math.round(n / 1024)} KiB`;
  return `${n} B`;
}

/** A source's state in words, and whether it is a problem. */
export function stateLabel(s: SourceState): { text: string; tone: "ok" | "warn" | "plain" } {
  switch (s) {
    case "enabled":
      return { text: m.pull_state_enabled(), tone: "ok" };
    case "paused":
      return { text: m.pull_state_paused(), tone: "warn" };
    case "login_refused":
      return { text: m.pull_state_login_refused(), tone: "warn" };
    case "removed":
      return { text: m.pull_state_removed(), tone: "plain" };
  }
}

/**
 * How far a folder got: catching up (new mail, UIDs up to what the
 * server said last) and backfilling (old mail, down to UID 1).
 */
export function folderProgress(f: SourceFolder): {
  catchUp: string;
  backfill: string;
  done: boolean;
} {
  const top = f.uid_next > 0 ? f.uid_next - 1 : 0;
  const caughtUp = f.uid_next > 0 && f.high >= top;
  const catchUp =
    f.uid_next === 0
      ? m.pull_not_asked_yet()
      : caughtUp
        ? m.pull_caught_up()
        : m.pull_catching_up({ high: String(f.high), top: String(top) });
  const backfill =
    f.low === 1
      ? m.pull_backfill_done()
      : f.low > 1
        ? m.pull_backfill_down_to({ uid: String(f.low) })
        : m.pull_backfill_not_started();
  return { catchUp, backfill, done: caughtUp && f.low === 1 };
}
