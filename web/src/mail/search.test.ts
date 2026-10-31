import { describe, expect, it } from "vitest";

import { ApiError } from "@/api/client";

import {
  append,
  applyHint,
  echoDiffers,
  matching,
  pick,
  queryProblem,
  quoteValue,
  scopeOf,
  valueAt,
} from "./search";
import type { View } from "./view";

const view = (v: Partial<View>): View => ({ area: "conversations", dg: false, ...v });

describe("scopeOf", () => {
  it("writes a view's scope as the server echoes it", () => {
    expect(scopeOf(view({ prefix: "shop@example.com" }))).toBe("deliveredto:shop@example.com");
    expect(scopeOf(view({ label: "receipts" }))).toBe("label:receipts");
    expect(scopeOf(view({ label: 'to do "now"' }))).toBe(String.raw`label:"to do \"now\""`);
    expect(scopeOf(view({ area: "archived" }))).toBe("is:archived");
    expect(scopeOf(view({ area: "downgraded" }))).toBe("is:downgraded");
    expect(scopeOf(view({ area: "leanings" }))).toBe("triage:phishing:yes");
    expect(scopeOf(view({ area: "leanings", tr: "kind:spam" }))).toBe("triage:kind:spam");
  });
  it("leaves the zones and all mail unscoped (§12.9)", () => {
    for (const area of ["conversations", "notifications", "all", "drafts"] as const)
      expect(scopeOf(view({ area }))).toBeUndefined();
  });
  it("prefers the prefix or label picked over the area", () => {
    expect(scopeOf(view({ area: "archived", label: "x" }))).toBe("label:x");
  });
});

describe("quoteValue", () => {
  it("quotes only what would not read back as one word", () => {
    expect(quoteValue("a")).toBe("a");
    expect(quoteValue("标签")).toBe("标签");
    expect(quoteValue("a b")).toBe('"a b"');
    expect(quoteValue("f(x)")).toBe('"f(x)"');
    expect(quoteValue(String.raw`a\b`)).toBe(String.raw`a\b`);
    expect(quoteValue('"\\')).toBe('"\\"\\\\"');
    expect(quoteValue("")).toBe('""');
  });
});

describe("valueAt", () => {
  it("finds the value of a completable operator under the caret", () => {
    expect(valueAt("foo label:re", 12)).toEqual({ op: "label", typed: "re", start: 10, end: 12 });
    expect(valueAt("label:re foo", 7)).toEqual({ op: "label", typed: "re", start: 6, end: 8 });
    expect(valueAt("-IS:", 4)).toEqual({ op: "is", typed: "", start: 4, end: 4 });
    expect(valueAt("(class:ma)", 9)).toEqual({ op: "class", typed: "ma", start: 7, end: 9 });
  });
  it("reads a quoted value with spaces as one", () => {
    expect(valueAt('label:"to d', 11)).toEqual({ op: "label", typed: "to d", start: 6, end: 11 });
    expect(valueAt('label:"a b" x', 11)).toEqual({ op: "label", typed: "a b", start: 6, end: 11 });
  });
  it("offers nothing outside a completable value", () => {
    expect(valueAt("from:al", 7)).toBeUndefined();
    expect(valueAt("label:x ", 8)).toBeUndefined();
    expect(valueAt("label:x", 3)).toBeUndefined();
    expect(valueAt("", 0)).toBeUndefined();
  });
});

describe("matching", () => {
  const cs = [{ value: "archived" }, { value: "downgraded", label: "failed DMARC" }];
  it("keeps those containing the text, ignoring case", () => {
    expect(matching(cs, "ARCH").map((c) => c.value)).toEqual(["archived"]);
    expect(matching(cs, "dmarc").map((c) => c.value)).toEqual(["downgraded"]);
    expect(matching(cs, "").length).toBe(2);
  });
  it("closes once the value is complete", () => {
    expect(matching(cs, "archived")).toEqual([]);
  });
});

describe("pick", () => {
  it("replaces the value, quoting it, and moves past it", () => {
    const t = "foo label:re";
    expect(pick(t, valueAt(t, 12)!, "to do")).toEqual({ text: 'foo label:"to do" ', caret: 18 });
    const u = "label:re bar";
    expect(pick(u, valueAt(u, 8)!, "receipts")).toEqual({ text: "label:receipts bar", caret: 14 });
    const g = "(is:ar)";
    expect(pick(g, valueAt(g, 6)!, "archived").text).toBe("(is:archived)");
  });
});

describe("append", () => {
  it("adds an item with one space either side", () => {
    expect(append("", "is:sent")).toBe("is:sent ");
    expect(append("label:x  ", "is:sent")).toBe("label:x is:sent ");
  });
});

describe("echo", () => {
  it("differs beyond white space only", () => {
    expect(echoDiffers(" from:a   b ", "from:a b")).toBe(false);
    expect(echoDiffers("FROM:a", "from:a")).toBe(true);
    expect(echoDiffers("after:-7d", "after:2026-10-25T10:00:00Z")).toBe(true);
  });
  it("applies a hint to the item it names", () => {
    expect(applyHint("x form:alice", { term: "form:alice", suggest: "from:" })).toBe(
      "x from:alice",
    );
    expect(applyHint("-tag:a", { term: "tag:a", suggest: "label:" })).toBe("-label:a");
    expect(
      applyHint("hasattachment:yes x", { term: "hasattachment:yes", suggest: "has:attachment" }),
    ).toBe("has:attachment x");
    expect(applyHint("xform:a", { term: "form:a", suggest: "from:" })).toBeUndefined();
  });
});

const err = (code: string, detail: string) => new ApiError(400, detail, code as ApiError["code"]);

describe("queryProblem", () => {
  it("marks the item at the server's character position", () => {
    const q = "标签 after:2026-13-01 x";
    const p = queryProblem(err("invalid_param", "q: at character 4: after: bad"), q);
    expect(p?.detail).toBe("after: bad");
    expect(q.slice(p!.span!.start, p!.span!.end)).toBe("after:2026-13-01");
  });
  it("counts characters, not UTF-16 units", () => {
    const q = "😀 is:unread";
    const p = queryProblem(err("unsupported_filter", "q: at character 3: reserved"), q);
    expect(q.slice(p!.span!.start, p!.span!.end)).toBe("is:unread");
  });
  it("has no span for the query as a whole, and ignores other errors", () => {
    expect(queryProblem(err("invalid_param", "q: the query is too long"), "x")).toEqual({
      code: "invalid_param",
      detail: "the query is too long",
    });
    expect(queryProblem(err("invalid_param", "limit: too big"), "x")).toBeUndefined();
    expect(queryProblem(err("not_found", "q: x"), "x")).toBeUndefined();
    expect(queryProblem(new Error("q: x"), "x")).toBeUndefined();
  });
});
