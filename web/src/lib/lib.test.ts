import { describe, expect, it } from "vitest";

import { effectiveDowngrade, knownTotal, listParams, parseView, viewHref } from "@/mail/view";

import { listTime, localPart } from "./format";
import { creationOptions, fromB64url, requestOptions, toB64url } from "./webauthn";

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
    expect(new Uint8Array(o.excludeCredentials?.[0]?.id as ArrayBuffer)).toEqual(new Uint8Array([7, 8]));
    expect(o.rp.id).toBe("mail.example.com");
  });
  it("decodes request options without allowCredentials", () => {
    const o = requestOptions({ publicKey: { challenge: "AA", userVerification: "required" } });
    expect(o.allowCredentials).toBeUndefined();
    expect(o.userVerification).toBe("required");
  });
});

describe("view state", () => {
  it("round-trips through the query string", () => {
    const v = { prefix: "github@example.com", cls: "machine" as const, dg: "include" as const, q: "code", m: "01ABC" };
    expect(parseView(new URL(viewHref(v), "http://x").searchParams)).toEqual(v);
    expect(viewHref({ dg: "hide" })).toBe("/");
  });
  it("ignores junk", () => {
    expect(parseView(new URLSearchParams("class=spam&dg=maybe&q=%20"))).toEqual({ dg: "hide" });
  });
  it("keeps an anchor only with its message", () => {
    expect(parseView(new URLSearchParams("m=01ABC&a=part/1.2/para/7"))).toEqual({ dg: "hide", m: "01ABC", a: "part/1.2/para/7" });
    expect(parseView(new URLSearchParams("a=part/1"))).toEqual({ dg: "hide" });
    expect(parseView(new URLSearchParams("m=01ABC&a=part/0/para/1")).a).toBeUndefined();
    expect(parseView(new URLSearchParams("m=01ABC&a=javascript:x")).a).toBeUndefined();
    expect(viewHref({ dg: "hide", a: "part/1" })).toBe("/");
  });
  it("shows a prefix's downgraded mail", () => {
    expect(effectiveDowngrade({ dg: "hide" })).toBe("hide");
    expect(effectiveDowngrade({ dg: "hide", prefix: "k3x9q@example.com" })).toBe("include");
    expect(listParams({ dg: "only", cls: "human" }).toString()).toBe("class=human&downgraded=only");
  });
});

describe("list totals", () => {
  const ps = {
    messages: 10,
    downgraded: 3,
    prefixes: [{ zone: "example.com", prefix: "k3x9q", address: "k3x9q@example.com", claimed: false, label: "", first_seen: "", last_seen: "", messages: 2, downgraded: 2 }],
  };
  it("comes from the prefix aggregation", () => {
    expect(knownTotal({ dg: "hide" }, ps)).toBe(7);
    expect(knownTotal({ dg: "include" }, ps)).toBe(10);
    expect(knownTotal({ dg: "only" }, ps)).toBe(3);
    expect(knownTotal({ dg: "hide", prefix: "k3x9q@example.com" }, ps)).toBe(2);
  });
  it("is unknown where the aggregation does not count", () => {
    expect(knownTotal({ dg: "hide", cls: "human" }, ps)).toBeUndefined();
    expect(knownTotal({ dg: "hide", q: "x" }, ps)).toBeUndefined();
    expect(knownTotal({ dg: "hide" }, undefined)).toBeUndefined();
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
