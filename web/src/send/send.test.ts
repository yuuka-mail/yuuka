import { describe, expect, it } from "vitest";

import { ApiError } from "@/api/client";
import type { Message, SendingIdentity, View } from "@/api/types";
import { isQuiet, systemFacts } from "@/mail/facts";

import { parseDraftFilter } from "./drafts-list";
import { abilities, badAddrs, draftBody, holdLabel, onlyConfirmer, outboxMoves, outboxState, parseAddrs, problem, replyBody, replySubject, standing } from "./outgoing";

const identity = (over: Partial<SendingIdentity>): SendingIdentity => ({
  id: "i",
  address: "me@example.com",
  display_name: "",
  owner: { id: "p", kind: "human", name: "operator" },
  tier: "auto",
  granted: true,
  owner_capped: false,
  recipient_scope: "any",
  allowlist: [],
  limits: { max_per_hour: 10, max_per_day: 50 },
  suspended: false,
  sends: true,
  ...over,
});

const owned = (kind: string, name: string) => ({ identity_owner: { id: "x", kind, name } });

describe("standing", () => {
  it("says a send queues, within what", () => {
    expect(standing(identity({}))).toEqual({ sends: true, headline: "Send queues it", detail: "auto · to any recipient · 10/hour · 50/day" });
    expect(standing(identity({ recipient_scope: "allowlist", allowlist: ["example.org"] })).detail).toContain("allowlisted recipients (example.org)");
  });
  it("says why it would be held, by reason, with the server's detail as given", () => {
    const s = standing(identity({ sends: false, tier: "draft", hold_reason: "owner", hold_detail: "whatever the server says" }));
    expect(s).toEqual({ sends: false, headline: "Held as a draft: owner cap", detail: "whatever the server says" });
    expect(holdLabel("policy")).toBe("drafts only");
    expect(holdLabel("review")).toBe("review not available yet");
  });
  it("names the one person who may confirm a draft on another person's address", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: [] };
    expect(onlyConfirmer(owned("human", "alice"), s)).toBe("alice");
    expect(onlyConfirmer(owned("human", "operator"), s)).toBeUndefined();
    expect(onlyConfirmer(owned("agent", "arisu"), s)).toBeUndefined();
  });
});

describe("outbox states", () => {
  it("puts unknown in the words of §4.5", () => {
    expect(outboxState("unknown")).toEqual({ label: "may have been sent", tone: "warn" });
    expect(outboxState("dead").tone).toBe("bad");
    expect(outboxState("sent").tone).toBe("ok");
  });
  it("polls only what moves by itself", () => {
    expect(outboxMoves({ state: "queued" })).toBe(true);
    expect(outboxMoves({ state: "partial" })).toBe(false);
    expect(outboxMoves({ state: "partial", next_retry_at: "2026-01-01T00:00:00Z" })).toBe(true);
    expect(outboxMoves({ state: "unknown" })).toBe(false);
  });
});

describe("abilities", () => {
  it("offers resolving unknown delivery to a person with submission:send only", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: ["message:read", "delivery:read", "draft:create", "submission:send"] } as const;
    expect(abilities({ ...s, scopes: [...s.scopes] })).toEqual({ compose: true, outbox: true, resolve: true });
    expect(abilities({ ...s, principal: "agent:arisu", scopes: [...s.scopes] }).resolve).toBe(false);
    expect(abilities({ ...s, scopes: ["message:read"] })).toEqual({ compose: false, outbox: false, resolve: false });
  });
});

describe("composing", () => {
  it("reads addresses as typed", () => {
    expect(parseAddrs(" a@example.org, b@example.org;c@example.org\nd@example.org ")).toEqual(["a@example.org", "b@example.org", "c@example.org", "d@example.org"]);
    expect(badAddrs("a@example.org, Bob <b@example.org>, nobody")).toEqual(["Bob", "<b@example.org>", "nobody"]);
  });
  it("replies Re: once, quoting the text below room to write", () => {
    expect(replySubject("Lunch")).toBe("Re: Lunch");
    expect(replySubject("RE: Lunch")).toBe("RE: Lunch");
    const m = { body_text: "Friday?\n\n> earlier\n", from_name: "Bob", from_addr: "bob@example.org", received_at: "2026-10-30T10:02:00Z" } as Message;
    const body = replyBody(m);
    expect(body.startsWith("\n\nOn ")).toBe(true);
    expect(body.endsWith("Bob wrote:\n> Friday?\n>\n> > earlier\n")).toBe(true);
  });
  it("saves the fields as a draft body", () => {
    expect(draftBody({ from: "", to: "a@example.org", cc: "", bcc: "", subject: "s", body: "b", inReplyTo: "m" })).toEqual({
      from: undefined,
      to: ["a@example.org"],
      cc: [],
      bcc: [],
      subject: "s",
      body_markdown: "b",
      in_reply_to: "m",
    });
  });
  it("lists open drafts unless told otherwise", () => {
    expect(parseDraftFilter(undefined)).toEqual(["draft", "held"]);
    expect(parseDraftFilter("outbox,bogus")).toEqual(["outbox"]);
  });
});

describe("problems", () => {
  it("are told by their code, the detail only where it is specific", () => {
    expect(problem(new ApiError(409, "the draft has gone to the outbox", "conflict"))).toEqual({
      text: "Already submitted: the draft has gone to the outbox",
      detail: undefined,
      code: "conflict",
    });
    expect(problem(new ApiError(400, "from: required for a new thread", "invalid_body")).detail).toBe("from: required for a new thread");
    expect(problem(new TypeError("fetch failed")).text).toBe("Could not reach yuuka");
  });
});

const sentFacts = (state: string) =>
  ({
    auth: { verified: false, not_received: true, line: "none (sent by this system, never received)" },
    sent: { outbox_id: "o", from: "me@example.com", state },
    downgraded: false,
    stripped: {},
  }) as View["facts"];

describe("facts of mail we sent", () => {
  const m = { downgraded: false, quarantined: false, received_at: "2026-10-31T13:57:00Z", class: "unknown" } as Message;
  const f = sentFacts;
  it("are not unverified mail", () => {
    const facts = systemFacts(m, f("sent"));
    expect(facts.find((x) => x.label === "Auth")?.parts).toEqual([{ text: "none: sent by this system, never received", tone: "plain" }]);
    expect(facts.find((x) => x.label === "Sent")?.parts.map((p) => p.text)).toEqual(["by this system as me@example.com", "sent"]);
    expect(facts.some((x) => x.label === "Submitted")).toBe(true);
    expect(JSON.stringify(facts)).not.toContain("unverified");
  });
  it("stay one line until the outbox asks for a decision", () => {
    expect(isQuiet(m, f("queued"))).toBe(true);
    expect(isQuiet(m, f("sent"))).toBe(true);
    expect(isQuiet(m, f("unknown"))).toBe(false);
    expect(isQuiet(m, f("dead"))).toBe(false);
  });
});
