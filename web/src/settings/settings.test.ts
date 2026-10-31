import { describe, expect, it } from "vitest";

import type { AppliedGrant, ManagedGrant } from "@/api/types";

import { grantSummary, lowerLimit, lowerTiers, narrowerScopes, removeCommand, subsetOf, tightenBody, tightenings } from "./authority";

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
    expect(tightenings(applied()).map((t) => t.kind)).toEqual(["suspend", "tier", "tier", "scope", "scope", "limit", "limit", "limit", "hold"]);
    const tight = applied({ send_policy: "none", recipient_scope: "thread", suspended: true, limits: { max_per_hour: 0, max_per_day: 0, max_new_recipients_per_day: 0 } });
    expect(tightenings(tight)).toEqual([]);
    expect(tightenings(applied({ recipient_scope: "allowlist", allowlist: ["example.org"] })).some((t) => t.kind === "allowlist")).toBe(true);
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
    expect(tightenBody({ kind: "limit", field: "max_per_day" }, { limit: 20 })).toEqual({ max_per_day: 20 });
    expect(tightenBody({ kind: "hold" }, { holdMinutes: 30 })).toEqual({ hold_seconds: 1800 });
    expect(tightenBody({ kind: "suspend" }, {})).toEqual({ suspended: true });
  });
  it("says a grant in one line, and how to remove it on the host", () => {
    expect(grantSummary(applied({ limits: {} }))).toBe("review — held 10 min after the checks · to any recipient · no hourly limit · no daily limit · no limit on new recipients");
    expect(grantSummary(applied({ send_policy: "draft" }))).toBe("drafts only");
    const g = { host: "yuuka grant set --identity '*@example.com' --principal agent:arisu" } as ManagedGrant;
    expect(removeCommand(g)).toBe("yuuka grant rm --identity '*@example.com' --principal agent:arisu");
  });
});
