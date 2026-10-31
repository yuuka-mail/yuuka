import { describe, expect, it } from "vitest";

import type { Message, MessageSummary, View } from "@/api/types";

import { openable, shownExtraction, timeLeft } from "./extracted";
import { isQuiet, leakParts, strippedText, warnings } from "./facts";
import { claimedHost, linkTarget } from "./links";
import { attachmentHref, merge } from "./reading-data";

describe("link targets", () => {
  it("shows the real host and flags text naming another", () => {
    expect(
      linkTarget("https://login.evil.example.net/bank", "https://bank.example.com/login"),
    ).toEqual({
      href: "https://login.evil.example.net/bank",
      shown: "login.evil.example.net",
      mismatch: true,
    });
    expect(linkTarget("https://www.example.com/x", "example.com")?.mismatch).toBe(false);
    expect(linkTarget("https://example.com/", "Read more")?.mismatch).toBe(false);
  });
  it("keeps only the schemes the view keeps", () => {
    expect(linkTarget("javascript:alert(1)", "x")).toBeUndefined();
    expect(linkTarget("data:text/html,x", "x")).toBeUndefined();
    expect(linkTarget("not a url", "x")).toBeUndefined();
    expect(linkTarget("mailto:a%40b@example.com", "write")?.shown).toBe("a@b@example.com");
  });
  it("shows look-alike hosts as punycode", () => {
    expect(linkTarget("https://exаmple.com/", "example.com")).toMatchObject({
      shown: "xn--exmple-4nf.com",
      mismatch: true,
    });
  });
  it("reads the host a text claims", () => {
    expect(claimedHost("https://Bank.example.com/login")).toBe("bank.example.com");
    expect(claimedHost("example.org")).toBe("example.org");
    expect(claimedHost("Click here")).toBeUndefined();
    expect(claimedHost("see example.org now")).toBeUndefined();
  });
});

describe("facts", () => {
  it("splits warnings at their keyword", () => {
    const w = warnings({
      warnings: ["dmarc=fail: the From address is probably forged", "downgraded: treat as suspect"],
    } as View["facts"]);
    expect(w).toEqual([
      { key: "dmarc=fail", text: "the From address is probably forged", tone: "bad" },
      { key: "downgraded", text: "treat as suspect", tone: "warn" },
    ]);
  });
  it("treats a spoof as an error and a leak as a caution", () => {
    const w = warnings({
      warnings: [
        "spoof: claims to be example.net",
        "leak: a@example.com was not given to example.org",
      ],
    } as View["facts"]);
    expect(w.map((x) => x.tone)).toEqual(["bad", "warn"]);
    expect(leakParts({ to: "a@example.com", verdict: "leak" }).map((p) => p.text)).toEqual([
      "a@example.com",
      "leak",
      "from an unproven sender",
    ]);
    expect(leakParts({ to: "a@example.com", verdict: "spoof", domain: "example.net" })[1]).toEqual({
      text: "spoof",
      tone: "bad",
    });
  });
  it("keeps the box for anything to act on", () => {
    const m = { downgraded: false, quarantined: false } as Message;
    const auth = { verified: true, dmarc: "pass", line: "" };
    const f = { auth, downgraded: false, stripped: {} } as View["facts"];
    expect(isQuiet(m, f)).toBe(true);
    expect(isQuiet(m, undefined)).toBe(false);
    expect(isQuiet(m, { ...f, auth: { ...auth, verified: false, dmarc: undefined } })).toBe(false);
    expect(isQuiet(m, { ...f, auth: { ...auth, dmarc: "none" } })).toBe(false);
    expect(isQuiet(m, { ...f, auth: { ...auth, withheld: true } })).toBe(false);
    expect(isQuiet(m, { ...f, warnings: ["unverified: no trusted receiver"] })).toBe(false);
    expect(isQuiet({ ...m, quarantined: true }, f)).toBe(false);
  });
  it("counts what was left out", () => {
    expect(strippedText({ tracking_pixels: 1, hidden: 2 })).toBe(
      "1 tracking pixel, 2 hidden elements",
    );
    expect(strippedText({})).toBe("");
  });
});

const page = (parts: { anchor: string; n: number[] }[], more?: string): View => ({
  uri: "yuuka://message/x",
  view_version: 1,
  level: "part",
  facts: {} as View["facts"],
  content: {
    notice: "",
    from: {},
    subject: "s",
    snippet: "",
    more,
    parts: parts.map((p) => ({
      anchor: p.anchor,
      media_type: "text/plain",
      blocks: p.n.map((i) => ({ anchor: `${p.anchor}/para/${i}`, kind: "para" })),
    })),
  },
});

describe("reading pages", () => {
  it("joins a part cut across pages", () => {
    const r = merge(page([{ anchor: "part/1", n: [1, 2] }], "part/1/para/3"), [
      page([
        { anchor: "part/1", n: [3] },
        { anchor: "part/2", n: [1] },
      ]),
    ]);
    expect(r.parts.map((p) => p.blocks.map((b) => b.anchor))).toEqual([
      ["part/1/para/1", "part/1/para/2", "part/1/para/3"],
      ["part/2/para/1"],
    ]);
    expect(r.more).toBeUndefined();
    expect(r.content.parts).toBeUndefined();
  });
});

describe("attachment links", () => {
  it("point at the download endpoint with both segments encoded", () => {
    expect(attachmentHref("01M68JFX8Z9E5RB2VCA8TXVF4E", "1.3")).toBe(
      "/api/v1/messages/01M68JFX8Z9E5RB2VCA8TXVF4E/attachments/1.3",
    );
    expect(attachmentHref("a/b", "../2")).toBe("/api/v1/messages/a%2Fb/attachments/..%2F2");
  });
});

describe("extracted values", () => {
  const now = Date.parse("2026-10-31T01:45:00Z");
  it("count down to the expiry, then say expired", () => {
    expect(timeLeft("2026-10-31T01:45:30Z", now)).toEqual({ text: "30 s left", expired: false });
    expect(timeLeft("2026-10-31T01:51:30Z", now)).toEqual({ text: "7 min left", expired: false });
    expect(timeLeft("2026-10-31T05:45:00Z", now)).toEqual({ text: "4 h left", expired: false });
    expect(timeLeft("2026-11-03T01:45:00Z", now)).toEqual({ text: "3 d left", expired: false });
    expect(timeLeft("2026-10-31T01:45:00Z", now)).toEqual({ text: "expired", expired: true });
    expect(timeLeft("not a time", now).expired).toBe(true);
  });
  it("offer only web links, by the host they really go to", () => {
    expect(openable("https://app.example.com/auth/magic?t=1")).toEqual({
      href: "https://app.example.com/auth/magic?t=1",
      host: "app.example.com",
    });
    expect(openable("https://bücher.example/x")?.host).toBe("xn--bcher-kva.example");
    expect(openable("javascript:alert(1)")).toBeUndefined();
    expect(openable("mailto:a@example.com")).toBeUndefined();
  });
  it("show in notification rows only", () => {
    const x = {
      code: { kind: "otp", value: "482913", confidence: 95, source: "subject" },
      expired: false,
    } as const;
    const m = { class: "machine", extracted: x } as unknown as MessageSummary;
    expect(shownExtraction(m)).toBe(x);
    expect(shownExtraction({ ...m, class: "human" })).toBeUndefined();
  });
});
