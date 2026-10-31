import { describe, expect, it } from "vitest";

import type { TriageReport } from "@/api/types";

import {
  byQuestion,
  leanChoices,
  leanLabel,
  markLabel,
  notableLeanings,
  parseLean,
  pct,
  ranked,
} from "./triage";
import { leanOf, listParams, parseView, viewHref } from "./view";

describe("estimates", () => {
  it("never round a probability to certainty", () => {
    expect(pct(0.999)).toBe(">99%");
    expect(pct(0.004)).toBe("<1%");
    expect(pct(0.5)).toBe("50%");
    expect(pct(0.856)).toBe("86%");
  });
  it("rank the options, most probable first", () => {
    expect(ranked({ question: "phishing", p: { yes: 0.1, no: 0.9 } })).toEqual([
      { option: "no", p: 0.9 },
      { option: "yes", p: 0.1 },
    ]);
    // An abstention keeps no probabilities.
    expect(ranked({ question: "phishing", abstained: true })).toEqual([]);
  });
  it("order questions as the set does, unknown ones last by name", () => {
    const qs = ["zeta", "needs_reply", "alpha", "kind", "phishing"].map((question) => ({
      question,
    }));
    expect(byQuestion(qs).map((q) => q.question)).toEqual([
      "kind",
      "phishing",
      "needs_reply",
      "alpha",
      "zeta",
    ]);
  });
  it("mark only the leanings worth a glance", () => {
    expect(notableLeanings(undefined)).toEqual([]);
    expect(notableLeanings({ kind: "notification", phishing: "no" })).toEqual([]);
    expect(notableLeanings({ kind: "spam", phishing: "yes", needs_reply: "yes" })).toEqual([
      { question: "phishing", option: "yes" },
      { question: "kind", option: "spam" },
    ]);
  });
  it("say a yes as the question, a choice as the option", () => {
    expect(markLabel({ question: "phishing", option: "yes" })).toBe("phishing?");
    expect(markLabel({ question: "kind", option: "spam" })).toBe("spam?");
    // Keys it does not know show as themselves, like any identifier.
    expect(leanLabel({ question: "tone", option: "angry" })).toBe("tone: angry");
  });
  it("read the filter only in its own grammar", () => {
    expect(parseLean("phishing:yes")).toEqual({ question: "phishing", option: "yes" });
    for (const bad of ["phishing", "Phishing:yes", "phishing:", ":yes", "a:b:c", ""]) {
      expect(parseLean(bad)).toBeUndefined();
    }
  });
  it("offer what the report counted, and keep the current choice", () => {
    const r: TriageReport = {
      scorer: "fake",
      questions: 1,
      scored: 10,
      unscored: 0,
      caveat: "",
      answers: [
        { question: "phishing", options: [{ option: "no", messages: 9 }], abstained: 1 },
        { question: "kind", options: [{ option: "spam", messages: 2 }], abstained: 0 },
      ],
    };
    expect(leanChoices(r, { question: "phishing", option: "yes" })).toEqual([
      { question: "phishing", option: "yes" },
      { question: "kind", option: "spam", messages: 2 },
      { question: "phishing", option: "no", messages: 9 },
    ]);
  });
});

describe("the leanings view", () => {
  it("lists phishing first, and every message whatever its zone", () => {
    const v = parseView(new URLSearchParams("area=leanings"));
    expect(leanOf(v)).toBe("phishing:yes");
    expect(listParams(v)).toEqual({ triage: "phishing:yes", downgraded: "include" });
    const spam = parseView(new URLSearchParams("area=leanings&tr=kind:spam"));
    expect(listParams(spam)).toEqual({ triage: "kind:spam", downgraded: "include" });
    expect(viewHref(spam)).toBe("/?area=leanings&tr=kind%3Aspam");
  });
  it("drops a filter it cannot read", () => {
    expect(parseView(new URLSearchParams("area=leanings&tr=DROP")).tr).toBeUndefined();
  });
  it("leaves other lists alone", () => {
    expect(leanOf(parseView(new URLSearchParams("q=invoice")))).toBeUndefined();
  });
});
