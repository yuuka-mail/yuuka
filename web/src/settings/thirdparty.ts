import type { Event, LockReason, ManagedTransport, SecretStatus, TransportCap } from "@/api/types";
import { m } from "@/paraglide/messages.js";

// What the transport, channel and notification settings decide before
// they ask (§17.6). The server decides again; these say what a change
// will cost, so the form can say it before the passkey prompt. Pure, so
// tested.

/** Every cap, in the order the server lists them (store.transportCaps). */
export const allCaps: TransportCap[] = ["message-id", "return-path", "dkim", "status-query"];

/**
 * The caps presets of `yuuka transport add --caps` (cmd/yuuka parseCaps):
 * Cloudflare Email Sending sets everything itself; a submission server
 * that delivers what it is given keeps our Message-ID and reverse-path.
 */
export const capsPresets = {
  cloudflare: [] as TransportCap[],
  standard: ["message-id", "return-path"] as TransportCap[],
};
export type CapsPreset = keyof typeof capsPresets | "custom";

/** The preset caps are, or custom. Order does not matter; the server stores them without repeats. */
export function presetOf(caps: TransportCap[]): CapsPreset {
  const set = new Set(caps);
  for (const [name, preset] of Object.entries(capsPresets)) {
    if (preset.length === set.size && preset.every((c) => set.has(c))) return name as CapsPreset;
  }
  return "custom";
}

/** Where a transport connects, as its audience would: user@host:port. */
export function destination(t: { host?: string; port?: number; username?: string }): string {
  const host = t.host?.includes(":") ? `[${t.host}]` : (t.host ?? "");
  const d = `${host}:${t.port ?? 465}`;
  return t.username ? `${t.username}@${d}` : d;
}

/** A transport form as typed. */
export interface TransportForm {
  name: string;
  host: string;
  port: string;
  username: string;
  password: string;
  caps: TransportCap[];
}

/** The form of a transport: its configuration, or empty for a new one or a placeholder. Never a password. */
export function transportForm(t?: ManagedTransport): TransportForm {
  const configured = t && t.state !== "placeholder";
  return {
    name: t?.name ?? "",
    host: configured ? (t.host ?? "") : "",
    port: configured && t.port ? String(t.port) : "465",
    username: configured ? (t.username ?? "") : "",
    password: "",
    caps: configured ? t.caps : [],
  };
}

/** Whether the server will accept the form's port and name. */
export function validTransportForm(f: TransportForm): boolean {
  const port = Number(f.port);
  return (
    /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(f.name) &&
    f.host.trim() !== "" &&
    Number.isInteger(port) &&
    port >= 1 &&
    port <= 65535
  );
}

/**
 * Whether saving f over t needs a password the form does not have: a
 * transport that authenticates and has none, or one that moves to
 * another host, port or user name (a password is only ever sent where
 * it was set for, §17.4). Mirrors manage.CheckPassword.
 */
export function needsPassword(t: ManagedTransport | undefined, f: TransportForm): boolean {
  if (!f.username || f.password) return false;
  // A placeholder and a removed transport have no password to keep.
  return (
    !t?.secret ||
    t.host !== f.host.trim().toLowerCase() ||
    t.port !== Number(f.port) ||
    (t.username ?? "") !== f.username
  );
}

/**
 * Whether moving an identity's chain from was to names lets its mail go
 * anywhere new: another primary, or a transport not in the chain. Taking
 * a fallback away or reordering the fallbacks only narrows where mail
 * goes, and takes no passkey (manage.ChainWidens).
 */
export function chainWidens(was: string[], names: string[]): boolean {
  if (names.length === 0) return false;
  if (was.length === 0 || names[0] !== was[0]) return true;
  return names.some((n) => !was.includes(n));
}

/** Why a secret is locked, for people. */
export function lockReason(r: LockReason | undefined): string {
  switch (r) {
    case "no_key":
      return m.secret_locked_no_key();
    case "key_missing":
      return m.secret_locked_key_missing();
    default:
      return m.secret_locked_set_again();
  }
}

/** A secret's state in a few words: ok, or locked and why. */
export function secretState(s: SecretStatus): { ok: boolean; text: string } {
  return s.state === "ok"
    ? { ok: true, text: m.secret_ok() }
    : { ok: false, text: lockReason(s.reason) };
}

/** Every event a binding can receive, in the order of §8.4. */
export const allEvents: Event[] = [
  "mail.new",
  "send.held",
  "send.escalated",
  "grant.suspended",
  "account.trouble",
];

export function eventLabel(e: Event): string {
  switch (e) {
    case "mail.new":
      return m.event_mail_new();
    case "send.held":
      return m.event_send_held();
    case "send.escalated":
      return m.event_send_escalated();
    case "grant.suspended":
      return m.event_grant_suspended();
    case "account.trouble":
      return m.event_account_trouble();
  }
}

/** The chat kind Telegram reported, for people. */
export function chatKind(kind: string): string {
  switch (kind) {
    case "private":
      return m.bind_chat_private();
    case "group":
    case "supergroup":
      return m.bind_chat_group();
    default:
      return kind || m.bind_chat_unknown();
  }
}
