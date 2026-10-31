import { describe, expect, it } from "vitest";

import { labelProblem } from "./label-input";
import { actionsComplete, asMatch, fromEdit, newLeaf, ruleActionsOf, splitValues, toEdit } from "./match";

describe("match trees", () => {
  const tree = {
    all: [
      { field: "from.addr", op: "eq", value: "a@example.com" },
      { not: { any: [{ field: "list.id", op: "in", value: ["x", "y"] }, { field: "attachment", op: "eq", value: true }] } },
    ],
  };
  it("reads the store format and nothing else", () => {
    expect(asMatch(tree)).toEqual(tree);
    expect(asMatch({ all: [], any: [] })).toBeUndefined();
    expect(asMatch({ field: "subject", op: "eq" })).toBeUndefined();
    expect(asMatch([tree])).toBeUndefined();
    expect(asMatch({ not: { all: [{ nope: 1 }] } })).toBeUndefined();
  });
  it("round-trips through the editor", () => {
    const m = asMatch(tree);
    const e = m && toEdit(m);
    expect(e && fromEdit(e)).toEqual(tree);
  });
  it("leaves to JSON what the form cannot hold", () => {
    expect(toEdit({ not: { not: { field: "attachment", op: "eq", value: true } } })).toBeUndefined();
    expect(toEdit({ field: "header.x", op: "eq", value: "1" })).toBeUndefined();
    expect(toEdit({ field: "attachment", op: "contains", value: "1" })).toBeUndefined();
  });
  it("starts a condition with a value of its field's kind", () => {
    expect(newLeaf("from.verified")).toMatchObject({ op: "eq", value: true });
    expect(newLeaf("class")).toMatchObject({ op: "eq", value: "machine" });
    expect(newLeaf("extraction")).toMatchObject({ op: "has", value: "otp" });
    expect(newLeaf("to.addr")).toMatchObject({ op: "eq", value: "" });
  });
  it("splits a typed list", () => {
    expect(splitValues(" a@x, b@x\nc@x ,, ")).toEqual(["a@x", "b@x", "c@x"]);
  });
});

describe("actions", () => {
  it("needs one, each complete", () => {
    expect(actionsComplete([])).toBe(false);
    expect(actionsComplete([{ kind: "label", label: " " }])).toBe(false);
    expect(actionsComplete([{ kind: "archive" }, { kind: "label", label: "aws" }])).toBe(true);
  });
  it("turns annotations into what a rule can do, dropping take-backs", () => {
    expect(
      ruleActionsOf([
        { kind: "label", label: "aws" },
        { kind: "unlabel", label: "x" },
        { kind: "classify", class: "unknown" },
        { kind: "classify", class: "machine" },
        { kind: "archive" },
      ]),
    ).toEqual([{ kind: "label", label: "aws" }, { kind: "classify", class: "machine" }, { kind: "archive" }]);
  });
});

describe("labels", () => {
  it("refuses what the server refuses", () => {
    expect(labelProblem("aws")).toBeUndefined();
    expect(labelProblem("账单")).toBeUndefined();
    expect(labelProblem("x".repeat(65))).toBeDefined();
    expect(labelProblem("$seen")).toBeDefined();
    expect(labelProblem("\\Flagged")).toBeDefined();
    expect(labelProblem("a\tb")).toBeDefined();
  });
});
