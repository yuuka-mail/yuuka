import { describe, expect, it } from "vitest";

import { knownTotal, listParams, parseView, viewHref } from "@/mail/view";

import { listTime, localPart } from "./format";
import { cn } from "./utils";
import { creationOptions, fromB64url, requestOptions, toB64url } from "./webauthn";

describe("cn", () => {
  it("knows the type roles are sizes, not colours", () => {
    expect(cn("text-primary-foreground", "text-ui")).toBe("text-primary-foreground text-ui");
    expect(cn("text-small", "text-ui")).toBe("text-ui");
  });
});

describe("composer in the URL", () => {
  it("keeps one composer, a saved draft by its id", () => {
    expect(viewHref({ ...parseView(new URLSearchParams("m=x&reply=x")) })).toBe("/?m=x&reply=x");
    expect(viewHref({ ...parseView(new URLSearchParams("compose=1")), d: "d1" })).toBe("/?d=d1");
    expect(listParams(parseView(new URLSearchParams("area=outbox")))).toBeUndefined();
  });
});

describe("base64url", () => {
  it("round-trips every length", () => {
    for (let n = 0; n < 40; n++) {
      const bytes = Uint8Array.from({ length: n }, (_, i) => (i * 37 + n) & 0xff);
      const s = toB64url(bytes);
      expect(s).not.toMatch(/[+/=]/);
      expect(new Uint8Array(fromB64url(s))).toEqual(bytes);
    }
  });
  it("accepts padding", () => {
    expect(new Uint8Array(fromB64url("_-8="))).toEqual(new Uint8Array([0xff, 0xef]));
  });
});

describe("WebAuthn options", () => {
  it("decodes creation options from go-webauthn", () => {
    const o = creationOptions({
      publicKey: {
        rp: { id: "mail.example.com", name: "yuuka" },
        user: { id: "AQID", name: "operator", displayName: "operator" },
        challenge: "BAUG",
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        excludeCredentials: [{ type: "public-key", id: "Bwg" }],
      },
    });
    expect(new Uint8Array(o.user.id as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(new Uint8Array(o.challenge as ArrayBuffer)).toEqual(new Uint8Array([4, 5, 6]));
    expect(new Uint8Array(o.excludeCredentials?.[0]?.id as ArrayBuffer)).toEqual(
      new Uint8Array([7, 8]),
    );
    expect(o.rp.id).toBe("mail.example.com");
  });
  it("decodes request options without allowCredentials", () => {
    const o = requestOptions({ publicKey: { challenge: "AA", userVerification: "required" } });
    expect(o.allowCredentials).toBeUndefined();
    expect(o.userVerification).toBe("required");
  });
});

describe("view state", () => {
  const home = { area: "conversations" as const, dg: false };
  it("round-trips through the query string", () => {
    const v = {
      area: "all" as const,
      prefix: "github@example.com",
      dg: true,
      q: "code",
      m: "01ABC",
    };
    expect(parseView(new URL(viewHref(v), "http://x").searchParams)).toEqual(v);
    expect(viewHref(home)).toBe("/");
  });
  it("ignores junk", () => {
    expect(parseView(new URLSearchParams("area=spam&dg=maybe&q=%20"))).toEqual(home);
  });
  it("keeps an anchor only with its message", () => {
    expect(parseView(new URLSearchParams("m=01ABC&a=part/1.2/para/7"))).toEqual({
      ...home,
      m: "01ABC",
      a: "part/1.2/para/7",
    });
    expect(parseView(new URLSearchParams("a=part/1"))).toEqual(home);
    expect(parseView(new URLSearchParams("m=01ABC&a=part/0/para/1")).a).toBeUndefined();
    expect(parseView(new URLSearchParams("m=01ABC&a=javascript:x")).a).toBeUndefined();
    expect(viewHref({ ...home, a: "part/1" })).toBe("/");
  });
  it("puts unknown mail with conversations", () => {
    expect(listParams(home)).toEqual({
      class: ["human", "unknown"],
      downgraded: "hide",
      archived: false,
    });
    expect(listParams({ ...home, area: "notifications" })).toEqual({
      class: ["machine"],
      downgraded: "hide",
      archived: false,
    });
    expect(listParams({ ...home, area: "downgraded" })).toEqual({ downgraded: "only" });
    expect(listParams({ ...home, area: "unclaimed" })).toBeUndefined();
  });
  it("shows all of a prefix's mail", () => {
    expect(listParams({ ...home, area: "notifications", prefix: "k3x9q@example.com" })).toEqual({
      prefix: "k3x9q@example.com",
      downgraded: "include",
    });
  });
  it("keeps archived mail out of the zones only", () => {
    expect(listParams({ ...home, area: "archived" })).toEqual({
      archived: true,
      downgraded: "hide",
    });
    expect(listParams({ ...home, area: "all" })).toEqual({ downgraded: "hide" });
    expect(listParams({ ...home, label: "aws" })).toEqual({ label: "aws", downgraded: "include" });
    expect(listParams({ ...home, area: "proposals" })).toBeUndefined();
  });
  it("round-trips labels, proposals and settings", () => {
    const v = { ...home, label: "AWS", p: "01P", settings: "rules" as const, rule: "01R" };
    expect(parseView(new URL(viewHref(v), "http://x").searchParams)).toEqual(v);
    expect(parseView(new URLSearchParams("rule=01R")).rule).toBeUndefined();
    expect(parseView(new URLSearchParams("settings=root")).settings).toBeUndefined();
  });
});

describe("list totals", () => {
  const ps = {
    messages: 10,
    downgraded: 3,
    total: 1,
    prefixes: [
      {
        zone: "example.com",
        prefix: "github",
        address: "github@example.com",
        claimed: true,
        label: "",
        first_seen: "",
        last_seen: "",
        messages: 2,
        downgraded: 0,
        expected_domains: [],
        observed: [],
        leaks: 0,
        unverified: 0,
        spoofs: 0,
      },
    ],
  };
  const all = { area: "all" as const, dg: false };
  it("comes from the prefix aggregation", () => {
    expect(knownTotal(all, ps)).toBe(7);
    expect(knownTotal({ ...all, dg: true }, ps)).toBe(10);
    expect(knownTotal({ ...all, area: "downgraded" }, ps)).toBe(3);
    expect(knownTotal({ ...all, prefix: "github@example.com" }, ps)).toBe(2);
  });
  it("is unknown where the aggregation does not count", () => {
    expect(knownTotal({ ...all, area: "conversations" }, ps)).toBeUndefined();
    expect(knownTotal({ ...all, q: "x" }, ps)).toBeUndefined();
    expect(knownTotal({ ...all, prefix: "k3x9q@example.com" }, ps)).toBeUndefined();
    expect(knownTotal(all, undefined)).toBeUndefined();
  });
});

describe("format", () => {
  it("shortens list times", () => {
    const now = new Date(2026, 9, 31, 12, 0);
    expect(listTime(new Date(2026, 9, 31, 9, 5).toISOString(), now)).toBe("09:05");
    expect(listTime(new Date(2026, 2, 3, 9, 5).toISOString(), now)).toBe("Mar 3");
    expect(listTime(new Date(2025, 2, 3, 9, 5).toISOString(), now)).toBe("2025-03-03");
  });
  it("splits addresses", () => {
    expect(localPart("svc-shop@example.com")).toBe("svc-shop");
  });
});
