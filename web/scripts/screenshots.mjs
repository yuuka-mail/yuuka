// Screenshots of the WebUI for looking at a change (AGENTS.md: light,
// dark and narrow). Runs against a live `yuuka serve` with a seeded
// database (go run ./internal/devseed/cmd) and drives the real passkey
// ceremonies through Chrome's virtual authenticator.
//
//   YUUKA_URL=http://localhost:31483 \
//   ENROLL_URL='http://localhost:31483/enroll#yke_...' \  # from `yuuka admin enroll`
//   OUT=/tmp/shots CHROME=/usr/bin/google-chrome pnpm run screenshots
//
// Without ENROLL_URL only the signed-out pages are captured. Nothing here
// is committed output; the PNGs are for looking at.

import { mkdirSync } from "node:fs";
import { join } from "node:path";

import { chromium } from "playwright-core";

const base = process.env.YUUKA_URL ?? "http://localhost:31483";
const enrollURL = process.env.ENROLL_URL;
const out = process.env.OUT ?? "screenshots";
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({ executablePath: process.env.CHROME ?? "/usr/bin/google-chrome", headless: true });
const sizes = { wide: { width: 1440, height: 900 }, narrow: { width: 390, height: 844 } };
const problems = [];

async function page(scheme, size, storageState) {
  const ctx = await browser.newContext({ colorScheme: scheme, viewport: sizes[size], deviceScaleFactor: size === "narrow" ? 2 : 1, storageState });
  const p = await ctx.newPage();
  // A signed-out /auth/session is a 401 by design; anything else is a problem.
  p.on("console", (m) => m.type() === "error" && !/status of 401/.test(m.text()) && problems.push(`${scheme}/${size} ${p.url()}: ${m.text()}`));
  p.on("pageerror", (e) => problems.push(`${scheme}/${size} ${p.url()}: ${e.message}`));
  return p;
}

async function authenticator(p) {
  const cdp = await p.context().newCDPSession(p);
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: { protocol: "ctap2", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  });
}

async function shot(p, name) {
  await p.waitForLoadState("networkidle");
  await p.screenshot({ path: join(out, `${name}.png`) });
  process.stdout.write(name + "\n");
}

// Signed out.
for (const scheme of ["light", "dark"]) {
  for (const size of Object.keys(sizes)) {
    const p = await page(scheme, size);
    await p.goto(base + "/?m=01M68JFX8Z9E5RB2VCA8TXVF4E");
    await p.getByRole("button", { name: /sign in with passkey/i }).waitFor();
    await shot(p, `login-${scheme}-${size}`);
    await p.goto(base + "/recover");
    await shot(p, `recover-${scheme}-${size}`);
    await p.close();
  }
}

if (enrollURL) {
  // Enroll a passkey, then sign in with it; the session is reused below.
  const p = await page("light", "wide");
  await authenticator(p);
  await p.goto(enrollURL);
  await p.getByLabel(/name for this passkey/i).fill("screenshots");
  await shot(p, "enroll-light-wide");
  await p.getByRole("button", { name: /register passkey/i }).click();
  await p.getByText(/passkey registered/i).first().waitFor();
  await shot(p, "enrolled-light-wide");
  await p.goto(base + "/login");
  await p.getByRole("button", { name: /sign in with passkey/i }).click();
  await p.getByRole("listbox", { name: "Messages" }).waitFor();
  const state = await p.context().storageState();

  // The first hit for text, or the one with this subject: generated
  // variants reuse corpus bodies under other subjects.
  const find = async (text, subject) => {
    const r = await p.evaluate(async (t) => (await fetch(`/api/v1/search?downgraded=include&q=${encodeURIComponent(t)}`)).json(), text);
    return r.hits.find((h) => !subject || h.subject === subject)?.id;
  };
  const lunch = await find("usual place", "Re: Lunch on Friday?");
  const otp = await find("482913");
  const forged = await find("Verify your account");
  // The structured reading view (§12.1) over mail of different shapes,
  // and a deep link to one block of it.
  const readings = {
    newsletter: await find("This week at Example", "This week at Example"),
    quoted: await find("Trip plan", "Re: Trip plan"),
    "reply-html": await find("Release checklist", "Re: Release checklist"),
    phish: await find("confirm your account", "Urgent: confirm your account"),
  };
  const prefixes = await p.evaluate(async () => (await fetch("/api/v1/prefixes")).json());
  const discovered = prefixes.prefixes.find((x) => !x.claimed)?.address;
  await p.close();

  for (const scheme of ["light", "dark"]) {
    for (const size of Object.keys(sizes)) {
      const s = await page(scheme, size, state);
      const tag = `${scheme}-${size}`;
      await s.goto(base + "/");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      await shot(s, `list-${tag}`);
      await s.goto(base + `/?m=${lunch}`);
      await s.getByRole("heading", { name: /lunch on friday/i }).waitFor();
      await shot(s, `reading-${tag}`);
      await s.goto(base + `/?m=${otp}`);
      await s.getByText("yuuka").last().waitFor();
      await shot(s, `reading-otp-${tag}`);
      for (const [name, id] of Object.entries(readings)) {
        if (!id) continue;
        await s.goto(base + `/?m=${id}`);
        await s.locator('[id="part/1/para/1"], [id="part/1.2/para/1"]').first().waitFor();
        await shot(s, `reading-${name}-${tag}`);
      }
      if (readings["reply-html"]) {
        await s.goto(base + `/?m=${readings["reply-html"]}&a=part/1.2/para/4`);
        await s.locator('[id="part/1.2/para/4"]').waitFor();
        await shot(s, `reading-anchor-${tag}`);
      }
      await s.goto(base + "/?q=verification");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      await shot(s, `search-${tag}`);
      if (discovered) {
        await s.goto(base + `/?prefix=${encodeURIComponent(discovered)}&m=${forged}`);
        await s.getByRole("heading").first().waitFor();
        await shot(s, `prefix-${tag}`);
      }
      if (size === "narrow") {
        await s.goto(base + "/");
        await s.getByRole("button", { name: "Prefixes" }).click();
        await shot(s, `prefixes-${tag}`);
      }
      await s.close();
    }
  }
}

await browser.close();
if (problems.length) {
  console.error("console errors:\n" + problems.join("\n"));
  process.exitCode = 1;
}
