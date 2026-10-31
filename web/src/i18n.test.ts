import { describe, expect, it } from "vitest";

import * as messages from "@/paraglide/messages.js";
import { locales } from "@/paraglide/runtime.js";

// Guards of the message catalogue (§12.11). A missing translation does not
// fail the build (types follow the messages, not the languages) and falls
// back to English at run time, so the catalogues are compared here; and a
// string written straight into JSX would bypass the catalogue unnoticed,
// so text nodes are checked the way design.test.ts checks type sizes.

type Message = string | { declarations?: string[] }[];
const catalogues = import.meta.glob<Record<string, Message>>("../messages/*.json", {
  import: "default",
  eager: true,
});
const byLocale = Object.fromEntries(
  Object.entries(catalogues).map(([f, c]) => [f.replace(/^.*\/(.+)\.json$/, "$1"), c]),
);

const keys = (c: Record<string, Message>) => Object.keys(c).filter((k) => !k.startsWith("$"));

/** The inputs a message uses: {name} placeholders, or a variant message's declared inputs. */
function inputs(v: Message): string[] {
  if (typeof v === "string")
    return [...v.matchAll(/\{([a-zA-Z_]\w*)(?::[^}]*)?\}/g)].map((x) => x[1] ?? "");
  return v.flatMap((x) => (x.declarations ?? []).flatMap((d) => /^input (\w+)/.exec(d)?.[1] ?? []));
}

describe("message catalogues", () => {
  const en = byLocale.en ?? {};
  it("has English, the source, and Chinese", () => {
    expect(Object.keys(byLocale).toSorted()).toEqual(["en", "zh"]);
    expect(keys(en).length).toBeGreaterThan(100);
  });
  for (const [locale, c] of Object.entries(byLocale)) {
    if (locale === "en") continue;
    it(`${locale} translates every message and nothing else`, () => {
      expect(keys(c).toSorted()).toEqual(keys(en).toSorted());
    });
    it(`${locale} uses only the inputs English has`, () => {
      const wrong = keys(c).flatMap((k) => {
        const known = new Set(inputs(en[k] ?? ""));
        return inputs(c[k] ?? "")
          .filter((i) => !known.has(i))
          .map((i) => `${k}: {${i}}`);
      });
      expect(wrong).toEqual([]);
    });
  }
});

/** Inputs that give n for every name a message asks for. */
const every = (n: number) => new Proxy({}, { get: () => n });

// A message no variant of which matches compiles fine and then prints its
// own key; every one is rendered here, with a singular and a plural input.
describe("compiled messages", () => {
  it("render in every language", () => {
    const broken: string[] = [];
    for (const [key, fn] of Object.entries(messages)) {
      if (typeof fn !== "function") continue;
      for (const locale of locales) {
        for (const n of [1, 2]) {
          const out = (fn as (i: unknown, o: { locale: string }) => string)(every(n), { locale });
          if (out === key || out.includes("undefined"))
            broken.push(`${locale} ${key}(${n}): ${out}`);
        }
      }
    }
    expect(broken).toEqual([]);
  });
});

// Text between tags, outside {expressions}: a word in JSX is UI text and
// belongs in the catalogue. shadcn's copied-in base pieces carry none.
const sources = import.meta.glob<string>(["./**/*.tsx", "!./components/ui/**"], {
  query: "?raw",
  import: "default",
  eager: true,
});
const stripComments = (s: string) =>
  s.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
// Code between a > and a < (a comparison, a ternary) has punctuation a
// sentence of UI text would not. The product name and key names are
// not translated.
const textNode =
  /(?<![=-])>([^<>{}();=&|?]*[A-Za-z\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}][^<>{}();=&|?]*)</gu;
const names = new Set(["yuuka", "Ctrl+Enter"]);

describe("JSX text", () => {
  it("comes from the message catalogue", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(10);
    const found = Object.entries(sources).flatMap(([file, src]) =>
      [...stripComments(src).matchAll(textNode)]
        .map((x) => x[1]?.trim() ?? "")
        .filter((t) => !names.has(t))
        .map((t) => `${file}: ${t}`),
    );
    expect(found).toEqual([]);
  });
});
