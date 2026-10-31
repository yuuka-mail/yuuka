import { describe, expect, it } from "vitest";

// index.css clears Tailwind's own type scale, so a stray text-sm renders
// at the inherited size without any error. The roles (§12.8) are the only
// sizes; this catches the old names and arbitrary values, including in
// shadcn components copied in later.
const sources = import.meta.glob<string>("./**/*.tsx", { query: "?raw", import: "default", eager: true });
const offscale = /\btext-(?:xs|sm|base|lg|[2-9]?xl)(?![\w-])|\btext-\[[\d.]+(?:px|r?em)\]/g;

describe("type scale", () => {
  it("uses only the role sizes", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(10);
    const found = Object.entries(sources).flatMap(([file, src]) => (src.match(offscale) ?? []).map((m) => `${file}: ${m}`));
    expect(found).toEqual([]);
  });
});
