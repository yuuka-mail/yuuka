import { describe, expect, it } from "vitest";

import type { AppliedGrant, ManagedTransport } from "@/api/types";

import {
  grantBody,
  grantForm,
  grantSummary,
  lowerLimit,
  lowerTiers,
  narrowerScopes,
  subsetOf,
  tightenBody,
  tightenings,
  validCount,
} from "./authority";
import { chainWidens, destination, needsPassword, presetOf, transportForm } from "./thirdparty";

const applied = (over: Partial<AppliedGrant> = {}): AppliedGrant => ({
  send_policy: "review",
  recipient_scope: "any",
  allowlist: [],
  limits: { max_per_hour: 10, max_per_day: 50, max_new_recipients_per_day: 5 },
  hold_seconds: 600,
  suspended: false,
  ...over,
});

describe("tightening a grant", () => {
  it("offers only lower tiers and narrower scopes", () => {
    expect(lowerTiers("auto")).toEqual(["review", "draft", "none"]);
    expect(lowerTiers("none")).toEqual([]);
    expect(narrowerScopes("any")).toEqual(["thread", "allowlist"]);
    // thread and allowlist do not contain each other: either way is a widening.
    expect(narrowerScopes("thread")).toEqual([]);
    expect(narrowerScopes("allowlist")).toEqual([]);
  });
  it("lists every tighter change, and nothing when nothing is tighter", () => {
    expect(tightenings(applied()).map((t) => t.kind)).toEqual([
      "suspend",
      "tier",
      "tier",
      "scope",
      "scope",
      "limit",
      "limit",
      "limit",
      "hold",
    ]);
    const tight = applied({
      send_policy: "none",
      recipient_scope: "thread",
      suspended: true,
      limits: { max_per_hour: 0, max_per_day: 0, max_new_recipients_per_day: 0 },
    });
    expect(tightenings(tight)).toEqual([]);
    expect(
      tightenings(applied({ recipient_scope: "allowlist", allowlist: ["example.org"] })).some(
        (t) => t.kind === "allowlist",
      ),
    ).toBe(true);
  });
  it("takes a lower limit or a smaller allowlist only", () => {
    expect(lowerLimit(10, 9)).toBe(true);
    expect(lowerLimit(10, 10)).toBe(false);
    expect(lowerLimit(undefined, 1000)).toBe(true);
    expect(lowerLimit(10, -1)).toBe(false);
    expect(subsetOf(["example.org", "ops@example.net"], ["EXAMPLE.org"])).toBe(true);
    expect(subsetOf(["example.org"], ["example.org"])).toBe(false);
    expect(subsetOf(["example.org"], ["example.com"])).toBe(false);
  });
  it("sends one column at a time", () => {
    expect(tightenBody({ kind: "limit", field: "max_per_day" }, { limit: 20 })).toEqual({
      max_per_day: 20,
    });
    expect(tightenBody({ kind: "hold" }, { holdMinutes: 30 })).toEqual({ hold_seconds: 1800 });
    expect(tightenBody({ kind: "suspend" }, {})).toEqual({ suspended: true });
  });
  it("says a grant in one line", () => {
    expect(grantSummary(applied({ limits: {} }))).toBe(
      "review — held 10 min after the checks · to any recipient · no hourly limit · no daily limit · no limit on new recipients",
    );
    expect(grantSummary(applied({ send_policy: "draft" }))).toBe("drafts only");
  });
});

describe("the whole-row grant form", () => {
  it("starts a new grant at the column defaults", () => {
    expect(grantBody(grantForm("01ID", "agent:arisu"))).toEqual({
      identity: "01ID",
      principal: "agent:arisu",
      send_policy: "none",
      recipient_scope: "thread",
      allowlist: [],
      suspended: false,
    });
  });
  it("keeps what is stored, an empty number being the default", () => {
    const f = grantForm("01ID", "service:monitoring", {
      send_policy: "auto",
      recipient_scope: "allowlist",
      allowlist: ["ops@example.net", "example.org"],
      limits: { max_per_hour: 100 },
      hold_seconds: 1200,
      suspended_at: "2026-11-01T00:00:00Z",
    });
    expect(f.max_per_day).toBe("");
    expect(grantBody({ ...f, allowlist: "ops@example.net,\nexample.org  a@example.com" })).toEqual({
      identity: "01ID",
      principal: "service:monitoring",
      send_policy: "auto",
      recipient_scope: "allowlist",
      allowlist: ["ops@example.net", "example.org", "a@example.com"],
      max_per_hour: 100,
      hold_seconds: 1200,
      suspended: true,
    });
    expect(validCount("")).toBe(true);
    expect(validCount("12")).toBe(true);
    expect(validCount("-1")).toBe(false);
    expect(validCount("1.5")).toBe(false);
  });
});

const transport = (over: Partial<ManagedTransport> = {}): ManagedTransport => ({
  id: "t1",
  name: "relay",
  state: "configured",
  host: "smtp.example.com",
  port: 465,
  username: "u",
  caps: ["message-id", "return-path"],
  secret: { sealed_at: "2026-11-01T00:00:00Z", state: "ok" },
  revision: 3,
  created_at: "2026-11-01T00:00:00Z",
  used_by: [],
  ...over,
});

describe("transports and channels", () => {
  const t = transport;

  it("names the caps presets", () => {
    expect(presetOf([])).toBe("cloudflare");
    expect(presetOf(["return-path", "message-id"])).toBe("standard");
    expect(presetOf(["dkim"])).toBe("custom");
  });

  it("says where a transport goes, as its audience would", () => {
    expect(destination({ host: "smtp.example.com", port: 465, username: "u" })).toBe(
      "u@smtp.example.com:465",
    );
    expect(destination({ host: "2001:db8::1", port: 587 })).toBe("[2001:db8::1]:587");
  });

  it("asks for the password again when the transport moves", () => {
    const f = transportForm(t());
    expect(f.password).toBe("");
    expect(needsPassword(t(), f)).toBe(false);
    expect(needsPassword(t(), { ...f, host: "SMTP.example.com" })).toBe(false);
    expect(needsPassword(t(), { ...f, host: "elsewhere.example.com" })).toBe(true);
    expect(needsPassword(t(), { ...f, port: "587" })).toBe(true);
    expect(needsPassword(t(), { ...f, username: "v" })).toBe(true);
    expect(needsPassword(t(), { ...f, username: "" })).toBe(false);
    expect(needsPassword(t({ state: "placeholder", secret: undefined }), f)).toBe(true);
    expect(needsPassword(undefined, { ...f, password: "x" })).toBe(false);
  });

  it("widens a chain only where mail could go somewhere new", () => {
    expect(chainWidens(["a", "b"], ["a"])).toBe(false);
    expect(chainWidens(["a", "b", "c"], ["a", "c", "b"])).toBe(false);
    expect(chainWidens(["a"], ["a", "b"])).toBe(true);
    expect(chainWidens(["a", "b"], ["b"])).toBe(true);
  });
});
