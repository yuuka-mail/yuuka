import { describe, expect, it } from "vitest";

import { ApiError } from "@/api/client";
import type { Message, SendingIdentity, SessionInfo, View } from "@/api/types";
import { isQuiet, systemFacts } from "@/mail/facts";
import { m as msg } from "@/paraglide/messages.js";

import { parseDraftFilter } from "./drafts-list";
import {
  abilities,
  badAddrs,
  checkLabel,
  draftBody,
  duration,
  holdLabel,
  onlyConfirmer,
  outboxMoves,
  outboxState,
  parseAddrs,
  problem,
  recallable,
  replyBody,
  replySubject,
  resumeCommand,
  reviewText,
  shellWord,
  standing,
  strangerStanding,
} from "./outgoing";
import { untilSent } from "./review";

describe("held mail's countdown", () => {
  it("counts down to when it is sent, never to an expiry", () => {
    const now = Date.parse("2026-11-01T04:00:00Z");
    expect(untilSent("2026-11-01T04:56:00Z", now)).toEqual({ unit: "min", n: 56 });
    expect(untilSent("2026-11-01T04:00:30Z", now)).toEqual({ unit: "s", n: 30 });
    expect(untilSent("2026-11-01T07:00:00Z", now)).toEqual({ unit: "h", n: 3 });
    expect(untilSent("2026-11-01T03:59:00Z", now)).toBeUndefined();
    expect(msg.review_sends_in_min({ n: 56 })).toBe("sends in 56 min");
    expect(msg.review_sends_in_min({ n: 56 }, { locale: "zh" })).toBe("56 分钟后发出");
    expect(msg.review_recall_left_min({ n: 56 }, { locale: "zh" })).toBe("还有 56 分钟可撤回");
  });
});

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
    expect(holdLabel("review")).toBe("escalated by the review");
    expect(holdLabel("withdrawn")).toBe("recalled");
  });
  it("says the review tier goes to the checks, then waits its window", () => {
    const review = { tier: "review", sends: false, hold_reason: "review", hold_detail: "reviewed first: …" } as const;
    const s = standing(identity({ ...review, recipient_scope: "thread", limits: {}, hold_seconds: 600 }));
    expect(s).toEqual({ sends: true, headline: "Send runs the review", detail: "review — held 10 min after the checks · to the thread's participants" });
    expect(standing(identity({ ...review, hold_seconds: 3600 })).detail).toContain("held 1 h after the checks");
    expect(standing(identity({ ...review, hold_seconds: 0 })).detail).toContain("sent right after the checks");
    // Unset is the server's default window.
    expect(reviewText(undefined)).toBe("review — held 10 min after the checks");
  });
  it("names the one person who may confirm a draft on another person's address", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: [] };
    expect(onlyConfirmer(owned("human", "alice"), s)).toBe("alice");
    expect(onlyConfirmer(owned("human", "operator"), s)).toBeUndefined();
    expect(onlyConfirmer(owned("agent", "arisu"), s)).toBeUndefined();
  });
  it("says whose a From is when no usable identity covers it, and that it stops at the draft", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: [] };
    const person = strangerStanding(owned("human", "alice").identity_owner, s);
    expect(person).toMatchObject({ reason: "owner", note: "held: owner cap", headline: "Held: only alice can send as this address" });
    const agent = strangerStanding(owned("agent", "arisu").identity_owner, s);
    expect(agent).toMatchObject({ reason: "policy", note: "held: no grant" });
    expect(agent.detail).toContain("agent:arisu's");
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
    expect(abilities({ ...s, scopes: [...s.scopes] })).toMatchObject({ compose: true, outbox: true, resolve: true });
    expect(abilities({ ...s, principal: "agent:arisu", scopes: [...s.scopes] }).resolve).toBe(false);
    expect(abilities({ ...s, scopes: ["message:read"] })).toMatchObject({ compose: false, outbox: false, resolve: false });
  });
  it("offers rules and decisions to a person only", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: ["message:read", "message:annotate", "rule:manage"] } as const;
    expect(abilities({ ...s, scopes: [...s.scopes] })).toMatchObject({ annotate: true, decide: true, rules: true });
    expect(abilities({ ...s, principal: "agent:arisu", scopes: [...s.scopes] })).toMatchObject({ annotate: true, decide: false, rules: false });
    expect(abilities({ ...s, scopes: ["message:read", "message:annotate"] })).toMatchObject({ decide: true, rules: false });
  });
  it("offers who-may-do-what to a person's own session with admin, never a token", () => {
    const s = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: ["admin"] } as SessionInfo;
    expect(abilities(s).authority).toBe(true);
    expect(abilities({ ...s, credential_kind: "token" }).authority).toBe(false);
    expect(abilities({ ...s, principal: "agent:arisu" }).authority).toBe(false);
    expect(abilities({ ...s, scopes: ["message:read", "prefix:manage", "prefix:read"] })).toMatchObject({ authority: false, prefixes: true });
  });
  it("lets a person recall any held mail, anyone else only their own", () => {
    const person = { principal: "human:operator", credential_id: "c", credential_kind: "session", scopes: ["submission:send"] } as SessionInfo;
    const agent = { ...person, principal: "agent:arisu", credential_kind: "token" };
    const row = { state: "held", submitted_by: { id: "a", kind: "agent", name: "arisu" } } as const;
    expect(recallable(row, person, abilities(person))).toBe(true);
    expect(recallable(row, agent, abilities(agent))).toBe(true);
    expect(recallable({ ...row, submitted_by: { id: "b", kind: "service", name: "monitoring" } }, agent, abilities(agent))).toBe(false);
    expect(recallable({ ...row, state: "queued" }, person, abilities(person))).toBe(false);
    expect(recallable(row, { ...person, scopes: ["message:read"] }, abilities({ ...person, scopes: ["message:read"] }))).toBe(false);
  });
});

describe("host commands", () => {
  it("quote what a shell would expand, as the server does", () => {
    expect(shellWord("arisu@example.com")).toBe("arisu@example.com");
    expect(shellWord("*@example.com")).toBe("'*@example.com'");
    expect(shellWord("it's")).toBe(`'it'\\''s'`);
    expect(resumeCommand("*@example.com", "agent:arisu")).toBe("yuuka grant set --identity '*@example.com' --principal agent:arisu --resume");
  });
  it("names each review check", () => {
    expect(checkLabel("extracted_values")).toBe("Codes from incoming mail");
    expect(duration(45)).toBe("45 s");
    expect(duration(90)).toBe("2 min");
    expect(duration(7200)).toBe("2 h");
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
