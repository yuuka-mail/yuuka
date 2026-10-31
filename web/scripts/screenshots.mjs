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
  // variants reuse corpus bodies under other subjects. Received mail
  // only: a reply sent from here can carry the same subject.
  const find = async (text, subject) => {
    const r = await p.evaluate(async (t) => (await fetch(`/api/v1/search?downgraded=include&q=${encodeURIComponent(t)}`)).json(), text);
    return r.hits.find((h) => !h.outgoing && (!subject || h.subject === subject))?.id;
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
  // The attachment list, which links each file as a plain download.
  const invoice = await find("Invoice attached", "Invoice and logo");
  const prefixes = await p.evaluate(async () => (await fetch("/api/v1/prefixes")).json());
  const discovered = prefixes.prefixes.find((x) => !x.claimed)?.address;
  // Sending (§12.10), from whatever the database has: none of it is
  // required. The held shot presses Send only as an identity that holds
  // every submission (sends=false), so nothing ever reaches the outbox.
  const get = (path) => p.evaluate(async (u) => {
    const r = await fetch(u);
    return r.ok ? r.json() : undefined;
  }, path);
  const heldFrom = (await get("/api/v1/identities"))?.identities.find((i) => !i.sends && !i.address.startsWith("*@"))?.address;
  const agentDraft = (await get("/api/v1/drafts"))?.drafts.find((d) => d.author.kind !== "human")?.id;
  // The outbox needs delivery:read; without it the view is not offered.
  const outbox = await get("/api/v1/outbox?limit=1");
  const unknown = (await get("/api/v1/outbox?state=unknown&limit=1"))?.entries[0]?.id;
  const sent = (await get("/api/v1/messages?limit=200"))?.messages.find((m) => m.outgoing)?.id;
  // The leak check (§3.3): a claimed prefix whose mail leaked or was
  // spoofed, and one message of each kind.
  const flagged = prefixes.prefixes.find((x) => x.leaks + x.spoofs > 0)?.address;
  const offenses = flagged ? ((await get(`/api/v1/prefixes/${encodeURIComponent(flagged)}`))?.offenses ?? []) : [];
  const offending = { leak: offenses.find((o) => o.verdict === "leak")?.message.id, spoof: offenses.find((o) => o.verdict === "spoof")?.message.id };
  await p.close();

  for (const scheme of ["light", "dark"]) {
    for (const size of Object.keys(sizes)) {
      const s = await page(scheme, size, state);
      const tag = `${scheme}-${size}`;
      await s.goto(base + "/");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      await shot(s, `list-${tag}`);
      // The other sidebar entries of §12.9.
      for (const area of ["notifications", "downgraded"]) {
        await s.goto(base + `/?area=${area}`);
        await s.getByRole("listbox", { name: "Messages" }).waitFor();
        await shot(s, `area-${area}-${tag}`);
      }
      await s.goto(base + "/?area=unclaimed");
      await s.getByRole("list", { name: "Unclaimed prefixes" }).waitFor();
      await shot(s, `area-unclaimed-${tag}`);
      await s.goto(base + "/?area=leaks");
      await s.getByRole("heading", { name: "Leaks & spoofs" }).waitFor();
      await shot(s, `area-leaks-${tag}`);
      if (flagged) {
        await s.goto(base + `/?prefix=${encodeURIComponent(flagged)}`);
        await s.getByRole("list", { name: "Offending mail" }).waitFor();
        await shot(s, `prefix-leaks-${tag}`);
        for (const [verdict, id] of Object.entries(offending)) {
          if (!id) continue;
          await s.goto(base + `/?prefix=${encodeURIComponent(flagged)}&m=${id}`);
          await s.getByText("Prefix", { exact: true }).waitFor();
          await shot(s, `reading-${verdict}-${tag}`);
        }
      }
      await s.goto(base + `/?m=${lunch}`);
      await s.getByRole("heading", { name: /lunch on friday/i }).waitFor();
      await shot(s, `reading-${tag}`);
      // Quiet facts are one line until opened (§12.9).
      await s.getByRole("button", { expanded: false }).filter({ hasText: "yuuka" }).click();
      await s.getByRole("button", { name: /fold the facts/i }).waitFor();
      await shot(s, `reading-facts-open-${tag}`);
      await s.goto(base + `/?m=${otp}`);
      await s.getByText("yuuka").last().waitFor();
      await shot(s, `reading-otp-${tag}`);
      for (const [name, id] of Object.entries(readings)) {
        if (!id) continue;
        await s.goto(base + `/?m=${id}`);
        await s.locator('[id="part/1/para/1"], [id="part/1.2/para/1"]').first().waitFor();
        await shot(s, `reading-${name}-${tag}`);
      }
      if (invoice) {
        await s.goto(base + `/?m=${invoice}`);
        await s.getByRole("link", { name: /invoice\.pdf/i }).waitFor();
        await shot(s, `reading-attachments-${tag}`);
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
      // Sending: the composer, a reply, a held submission, drafts, the outbox.
      await s.goto(base + "/?compose=1");
      await s.getByRole("textbox", { name: "Body" }).waitFor();
      await shot(s, `compose-${tag}`);
      if (lunch) {
        await s.goto(base + `/?m=${lunch}&reply=${lunch}`);
        await s.getByRole("textbox", { name: "Body" }).waitFor();
        await shot(s, `compose-reply-${tag}`);
      }
      if (heldFrom) {
        await s.goto(base + "/?compose=1");
        await s.getByLabel("From", { exact: true }).selectOption(heldFrom);
        await s.getByLabel("To", { exact: true }).fill("someone@example.org");
        await s.getByLabel("Subject", { exact: true }).fill("Screenshot of a held submission");
        await s.getByRole("textbox", { name: "Body" }).fill("Held by the grant; never queued.");
        await s.getByText(/^Saved /).waitFor();
        await s.getByRole("button", { name: "Send", exact: true }).click();
        // The result box, not the preview line above From.
        await s.getByText("held as a draft", { exact: true }).waitFor();
        await shot(s, `compose-held-${tag}`);
        await s.getByRole("button", { name: "Delete draft" }).click();
        await s.getByRole("button", { name: "Delete draft" }).click();
        await s.getByRole("button", { name: "Delete draft" }).waitFor({ state: "detached" });
      }
      await s.goto(base + "/?area=drafts" + (agentDraft ? `&d=${agentDraft}` : ""));
      // A draft that an agent wrote opens in the composer, over the list on a phone.
      await (agentDraft ? s.getByRole("textbox", { name: "Body" }) : s.getByRole("heading", { name: "Drafts" })).waitFor();
      await shot(s, `drafts-${tag}`);
      if (outbox) {
        await s.goto(base + "/?area=outbox");
        await s.getByRole("heading", { name: "Outbox" }).waitFor();
        await shot(s, `outbox-${tag}`);
        if (unknown) {
          await s.goto(base + `/?area=outbox&o=${unknown}`);
          await s.getByRole("region", { name: "Delivery unknown" }).waitFor();
          await shot(s, `outbox-unknown-${tag}`);
        }
      }
      if (sent) {
        await s.goto(base + `/?m=${sent}`);
        await s.getByText(/sent as |by this system as /).first().waitFor();
        await shot(s, `reading-sent-${tag}`);
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
