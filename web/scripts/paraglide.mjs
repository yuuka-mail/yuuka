// The one place the message compiler is configured (§12.11): vite.config.ts
// runs it as a plugin, and `pnpm run i18n` (part of check) runs it on its
// own, so tsc sees the generated src/paraglide before it type-checks.
//
// The choice of language: the one picked in the UI (kept in this browser
// only), else the browser's, else English, the source and the fallback.

import { fileURLToPath } from "node:url";

import { compile } from "@inlang/paraglide-js";

const web = (p) => fileURLToPath(new URL(`../${p}`, import.meta.url));

/** @type {import("@inlang/paraglide-js").CompilerOptions} */
export const paraglide = {
  project: web("project.inlang"),
  outdir: web("src/paraglide"),
  strategy: ["localStorage", "preferredLanguage", "baseLocale"],
};

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  await compile(paraglide);
}
