import { describe, expect, it } from "vitest";

import type { ManagedSource, SourceFolder } from "@/api/types";

import {
  accountBody,
  accountForm,
  bytes,
  folderProgress,
  sourceDestination,
  sourceWidens,
  validAccountForm,
  type AccountForm,
} from "./pull";

// Not 2 << 30: JavaScript shifts in 32 bits.
const GiB = 1024 ** 3;

const src = (over: Partial<ManagedSource> = {}): ManagedSource => ({
  state: "enabled",
  address: "me@gmail.example.com",
  host: "imap.gmail.com",
  port: 993,
  username: "me@gmail.example.com",
  preset: "gmail",
  max_message_bytes: 32 << 20,
  daily_byte_budget: 2 * GiB,
  poll_seconds: 60,
  backfill_since: "2026-10-01T00:00:00Z",
  revision: 3,
  created_at: "2026-11-01T00:00:00Z",
  folders: [],
  failures: [],
  ...over,
});
const now = new Date("2026-11-01T12:00:00Z");
const folder = (high: number, low: number, uidNext: number): SourceFolder => ({
  name: "[Gmail]/All Mail",
  uidvalidity: 1,
  high,
  low,
  uid_next: uidNext,
  messages: 10,
  rematching: false,
  drafts_skipped: 0,
});

describe("pull accounts", () => {
  it("sends the form whole, with the source's own sizes and backfill kept", () => {
    const f = accountForm(src(), "gmail");
    expect(f.password).toBe("");
    expect(accountBody(f, src(), now)).toEqual({
      revision: 3,
      preset: "gmail",
      address: "me@gmail.example.com",
      host: "imap.gmail.com",
      port: 993,
      username: "me@gmail.example.com",
      backfill_since: "2026-10-01T00:00:00Z",
      max_message_bytes: 32 << 20,
      daily_byte_budget: 2 * GiB,
      poll_seconds: 60,
    });
    expect(accountBody({ ...f, backfill: "days", days: "7" }, src(), now).backfill_since).toBe(
      "2026-10-25T12:00:00.000Z",
    );
    expect(accountBody({ ...f, backfill: "all" }, src(), now).backfill_since).toBeUndefined();
    expect(accountBody({ ...f, budgetMiB: "0" }, src(), now).daily_byte_budget).toBe(0);
  });

  it("needs a name, an address, a password and a server for a new one", () => {
    const f = accountForm();
    expect(validAccountForm(f, true)).toBe(false);
    const ok: AccountForm = { ...f, name: "gmail", address: "me@example.com", password: "x" };
    expect(validAccountForm(ok, true)).toBe(true);
    expect(validAccountForm({ ...ok, preset: "" }, true)).toBe(false);
    expect(validAccountForm({ ...ok, preset: "", host: "imap.example.com" }, true)).toBe(true);
    expect(validAccountForm({ ...ok, port: "99999" }, true)).toBe(false);
    expect(validAccountForm({ ...ok, password: "" }, false)).toBe(true);
  });

  it("asks for the passkey only when more comes in or the password moves", () => {
    const f = accountForm(src(), "gmail");
    const widens = (patch: Partial<AccountForm>) =>
      sourceWidens(src(), accountBody({ ...f, ...patch }, src(), now));
    expect(sourceWidens(undefined, accountBody(f, undefined, now))).toBe(true);
    expect(widens({})).toBe(false);
    expect(widens({ poll: "300" })).toBe(false);
    expect(widens({ budgetMiB: "1024" })).toBe(false);
    expect(widens({ maxMiB: "16" })).toBe(false);
    expect(widens({ backfill: "none" })).toBe(false);
    expect(widens({ poll: "30" })).toBe(true);
    expect(widens({ budgetMiB: "0" })).toBe(true);
    expect(widens({ backfill: "all" })).toBe(true);
    expect(widens({ mailboxes: "INBOX" })).toBe(true);
    expect(widens({ authservId: "mx.google.com" })).toBe(true);
    expect(widens({ password: "new" })).toBe(true);
    expect(widens({ host: "imap.example.com" })).toBe(true);
  });

  it("says how far a folder got", () => {
    expect(folderProgress(folder(0, 0, 0))).toMatchObject({
      backfill: "old mail: not started",
      done: false,
    });
    expect(folderProgress(folder(40, 12, 101))).toEqual({
      catchUp: "new mail: UID 40 of 100",
      backfill: "old mail: down to UID 12",
      done: false,
    });
    expect(folderProgress(folder(100, 1, 101))).toMatchObject({
      catchUp: "new mail: caught up",
      done: true,
    });
  });

  it("writes sizes and destinations short", () => {
    expect(bytes(2 * GiB)).toBe("2 GiB");
    expect(bytes(5 * GiB)).toBe("5 GiB");
    expect(bytes(32 << 20)).toBe("32 MiB");
    expect(bytes(1536)).toBe("2 KiB");
    expect(sourceDestination(src())).toBe("me@gmail.example.com@imap.gmail.com:993");
  });
});
