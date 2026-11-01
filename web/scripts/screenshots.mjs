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
//
// Everything is captured in English, the source language, whose
// accessible names the steps below look for. The main screens are
// captured again in each other language (§12.11), found by role or by
// the accessible names of that language's catalogue.

import { mkdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { join } from "node:path";

import { chromium } from "playwright-core";

const base = process.env.YUUKA_URL ?? "http://localhost:31483";
const enrollURL = process.env.ENROLL_URL;
const out = process.env.OUT ?? "screenshots";
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({
  executablePath: process.env.CHROME ?? "/usr/bin/google-chrome",
  headless: true,
});
const sizes = { wide: { width: 1440, height: 900 }, narrow: { width: 390, height: 844 } };
const problems = [];

// The other UI languages; English is the default and needs nothing set.
// Their screens are found by the accessible names in their own catalogue.
const translations = ["zh"];
const catalogues = Object.fromEntries(
  await Promise.all(
    translations.map(async (l) => [
      l,
      JSON.parse(await readFile(new URL(`../messages/${l}.json`, import.meta.url), "utf8")),
    ]),
  ),
);

async function page(scheme, size, storageState, lang) {
  const ctx = await browser.newContext({
    colorScheme: scheme,
    viewport: sizes[size],
    deviceScaleFactor: size === "narrow" ? 2 : 1,
    storageState,
  });
  // The language choice the account menu stores (Paraglide's localStorage strategy).
  if (lang) await ctx.addInitScript((l) => localStorage.setItem("PARAGLIDE_LOCALE", l), lang);
  // The copy button of an extracted code writes to the clipboard.
  await ctx.grantPermissions(["clipboard-read", "clipboard-write"], {
    origin: new URL(base).origin,
  });
  const p = await ctx.newPage();
  // A signed-out /auth/session is a 401 by design, and so is a 400 for a
  // query the search screens show going wrong; anything else is a problem.
  p.on(
    "console",
    (m) =>
      m.type() === "error" &&
      !/status of 401/.test(m.text()) &&
      !(/status of 400/.test(m.text()) && m.location().url.includes("/api/v1/search")) &&
      problems.push(`${scheme}/${size} ${p.url()}: ${m.text()}`),
  );
  p.on("pageerror", (e) => problems.push(`${scheme}/${size} ${p.url()}: ${e.message}`));
  return p;
}

async function authenticator(p) {
  const cdp = await p.context().newCDPSession(p);
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
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
  await p
    .getByText(/passkey registered/i)
    .first()
    .waitFor();
  await shot(p, "enrolled-light-wide");
  await p.goto(base + "/login");
  await p.getByRole("button", { name: /sign in with passkey/i }).click();
  await p.getByRole("listbox", { name: "Messages" }).waitFor();
  const state = await p.context().storageState();

  // The first hit for text, or the one with this subject: generated
  // variants reuse corpus bodies under other subjects. Received mail
  // only: a reply sent from here can carry the same subject.
  const find = async (text, subject) => {
    const r = await p.evaluate(
      async (t) =>
        (await fetch(`/api/v1/search?downgraded=include&q=${encodeURIComponent(t)}`)).json(),
      text,
    );
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
  // Mail of the first pull account, if the database has one (§9.5).
  const pullAccount = (
    await p.evaluate(async () => (await fetch("/api/v1/accounts")).json())
  ).accounts.find((a) => a.kind === "imap")?.name;
  const pulled = pullAccount
    ? (
        await p.evaluate(
          async (q) =>
            (
              await fetch(`/api/v1/search?downgraded=include&limit=2&q=${encodeURIComponent(q)}`)
            ).json(),
          `account:${pullAccount}`,
        )
      ).hits.map((h) => h.id)
    : [];
  const prefixes = await p.evaluate(async () => (await fetch("/api/v1/prefixes")).json());
  const discovered = prefixes.prefixes.find((x) => !x.claimed)?.address;
  // Sending (§12.10), from whatever the database has: none of it is
  // required. The held shot presses Send only as an identity that holds
  // every submission (outcome draft_only), so nothing ever reaches the outbox.
  const get = (path) =>
    p.evaluate(async (u) => {
      const r = await fetch(u);
      return r.ok ? r.json() : undefined;
    }, path);
  const heldFrom = (await get("/api/v1/identities"))?.identities.find(
    (i) => i.outcome === "draft_only" && !i.address.startsWith("*@"),
  )?.address;
  const agentDraft = (await get("/api/v1/drafts"))?.drafts.find(
    (d) => d.author.kind !== "human",
  )?.id;
  // The outbox needs delivery:read; without it the view is not offered.
  const outbox = await get("/api/v1/outbox?limit=1");
  const unknown = (await get("/api/v1/outbox?state=unknown&limit=1"))?.entries[0]?.id;
  const sent = (await get("/api/v1/messages?limit=200"))?.messages.find((m) => m.outgoing)?.id;
  // The leak check (§3.3): a claimed prefix whose mail leaked or was
  // spoofed, and one message of each kind.
  const flagged = prefixes.prefixes.find((x) => x.leaks + x.spoofs > 0)?.address;
  const offenses = flagged
    ? ((await get(`/api/v1/prefixes/${encodeURIComponent(flagged)}`))?.offenses ?? [])
    : [];
  const offending = {
    leak: offenses.find((o) => o.verdict === "leak")?.message.id,
    spoof: offenses.find((o) => o.verdict === "spoof")?.message.id,
  };
  // Annotations, rules and proposals (§8.5, §12.12), from whatever the
  // database has: a labelled message, a label, the rules, the proposals.
  const labelled = (await get("/api/v1/messages?limit=200"))?.messages.find(
    (x) => x.labels.length > 0,
  );
  const label = labelled?.labels[0];
  const rules = (await get("/api/v1/rules"))?.rules ?? [];
  const proposals = (await get("/api/v1/proposals?state=pending"))?.proposals ?? [];
  const ruleProposal = proposals.find((x) => x.kind === "rule" && x.message_id);
  const annotateProposal = proposals.find((x) => x.kind === "annotate" && x.message_id);
  // The review tier (§8.9): mail held for its recall window, a draft the
  // checks escalated, and an identity whose submissions go to the checks.
  // None of it is required; the shots that need it are skipped without.
  const held = (await get("/api/v1/outbox?state=held&limit=1"))?.entries[0]?.id;
  const escalated = (await get("/api/v1/drafts?state=held"))?.drafts.find(
    (d) => d.hold_reason === "review" && d.review,
  )?.id;
  const reviewFrom = (await get("/api/v1/identities"))?.identities.find(
    (i) => i.outcome === "held_after_review" && !i.address.startsWith("*@"),
  )?.address;
  // Notes (§8.10): the first of these messages that has any.
  let noted;
  for (const id of [lunch, otp]) {
    if (id && !noted && (await get(`/api/v1/messages/${id}`))?.notes?.length) noted = id;
  }
  // Who may do what (§13.10): only a person's own session with admin sees it.
  const admin = (await get("/auth/session"))?.scopes.includes("admin");
  const claimed = prefixes.prefixes.find((x) => x.claimed)?.address;
  // A scorer's estimates (§15), when one is configured (the counts say);
  // without one none of the triage shots are taken. Scores come from
  // whatever scorer the server was started with.
  const triage = (await get("/api/v1/counts"))?.triage;
  const leaning =
    triage &&
    (await get("/api/v1/messages?triage=phishing:yes&downgraded=include&limit=1"))?.messages[0];
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
      // The code is the row in the notification zone (§12.9): copied
      // with one press, which says so for a moment.
      await s.goto(base + "/?area=notifications");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      const copy = s.getByRole("button", { name: /^Copy / }).first();
      if (await copy.count()) {
        await copy.click();
        await s.getByRole("button", { name: "Copied" }).first().waitFor();
        await shot(s, `area-notifications-copied-${tag}`);
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
      // Mail pulled from a mailbox (§9.5), header only or whole.
      for (const [i, id] of pulled.entries()) {
        await s.goto(base + `/?m=${id}`);
        await s.getByText("pulled from").first().waitFor();
        await shot(s, `reading-pulled-${i + 1}-${tag}`);
      }
      if (readings["reply-html"]) {
        await s.goto(base + `/?m=${readings["reply-html"]}&a=part/1.2/para/4`);
        await s.locator('[id="part/1.2/para/4"]').waitFor();
        await shot(s, `reading-anchor-${tag}`);
      }
      await s.goto(base + "/?q=verification");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      await shot(s, `search-${tag}`);
      // The box is the query (§16.5): a view's scope prefilled in it, values
      // offered as typed, the operators one press away, how yuuka read the
      // query and where it could not.
      const box = s.getByRole("combobox", { name: "Search mail" });
      if (claimed) {
        await s.goto(base + `/?prefix=${encodeURIComponent(claimed)}`);
        await s.getByRole("listbox", { name: "Messages" }).waitFor();
        await box.focus();
        await s.keyboard.type("invoice");
        await s.getByRole("heading", { name: /deliveredto:/ }).waitFor();
        await shot(s, `search-prefill-${tag}`);
      }
      await s.goto(base + "/");
      await s.getByRole("listbox", { name: "Messages" }).waitFor();
      await box.fill("is:a");
      await s.getByRole("listbox", { name: "Values for is:" }).waitFor();
      await shot(s, `search-complete-${tag}`);
      await box.fill("");
      await s.getByRole("button", { name: "Search operators" }).click();
      await s.getByRole("dialog", { name: "Search operators" }).waitFor();
      await shot(s, `search-help-${tag}`);
      await s.goto(base + "/?q=" + encodeURIComponent("FROM:example form:alice after:-7d"));
      await s.getByRole("button", { name: "Use from:" }).waitFor();
      await shot(s, `search-echo-${tag}`);
      await s.goto(base + "/?q=" + encodeURIComponent("invoice after:2026-13-01"));
      await s.getByRole("alert").waitFor();
      await shot(s, `search-error-${tag}`);
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
      await (
        agentDraft
          ? s.getByRole("textbox", { name: "Body" })
          : s.getByRole("heading", { name: "Drafts" })
      ).waitFor();
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
        await s
          .getByText(/sent as |by this system as /)
          .first()
          .waitFor();
        await shot(s, `reading-sent-${tag}`);
      }
      if (size === "narrow") {
        await s.goto(base + "/");
        await s.getByRole("button", { name: "Prefixes" }).click();
        await shot(s, `prefixes-${tag}`);
      }
      // Labels, archive and where a message's state came from (§8.5).
      await s.goto(base + "/?area=archived");
      await s.getByRole("heading", { name: "Archived" }).waitFor();
      await shot(s, `area-archived-${tag}`);
      if (label) {
        await s.goto(base + `/?label=${encodeURIComponent(label)}`);
        await s.getByRole("listbox", { name: "Messages" }).waitFor();
        await shot(s, `label-${tag}`);
      }
      if (labelled) {
        await s.goto(base + `/?m=${labelled.id}`);
        await s.getByRole("button", { name: "Why it is here" }).click();
        await s
          .getByRole("region", { name: "Where its labels, zone and archived state came from" })
          .getByRole("listitem")
          .first()
          .waitFor();
        await shot(s, `reading-history-${tag}`);
        await s.getByRole("button", { name: "Label", exact: true }).click();
        await s.getByRole("textbox", { name: "Label" }).fill(label.slice(0, 2));
        await shot(s, `reading-label-input-${tag}`);
      }
      // Moving a message between zones offers a rule and creates none
      // (§12.9); the move is undone again afterwards.
      if (lunch) {
        await s.goto(base + `/?m=${lunch}`);
        await s.getByRole("button", { name: "Move to Notifications" }).click();
        await s.getByRole("button", { name: "Draft a rule" }).waitFor();
        await shot(s, `reading-reclassified-${tag}`);
        await s.getByRole("button", { name: "Draft a rule" }).click();
        await s
          .getByRole("region", { name: "What the rule would do" })
          .getByText(/^Matches /)
          .waitFor();
        await shot(s, `reading-drafter-${tag}`);
        await s.getByRole("button", { name: "Undo" }).first().click();
        await s.getByText(/^undone: /).waitFor();
      }
      // Proposals: the attention entry, and the card beside the mail.
      await s.goto(base + "/?area=proposals");
      await s.getByRole("list", { name: "Proposals" }).waitFor();
      await shot(s, `area-proposals-${tag}`);
      if (annotateProposal) {
        await s.goto(
          base + `/?area=proposals&p=${annotateProposal.id}&m=${annotateProposal.message_id}`,
        );
        await s.getByRole("button", { name: "Accept and automate…" }).click();
        await s
          .getByRole("region", { name: "Rule draft" })
          .getByText(/^Matches /)
          .waitFor();
        await shot(s, `proposal-automate-${tag}`);
      }
      if (ruleProposal) {
        await s.goto(base + `/?area=proposals&p=${ruleProposal.id}&m=${ruleProposal.message_id}`);
        await s.getByRole("button", { name: "Review rule…" }).click();
        await s
          .getByRole("region", { name: "What the rule would do" })
          .getByText(/^Matches /)
          .waitFor();
        await shot(s, `proposal-rule-${tag}`);
      }
      // Settings and the rules in it (§12.12), from the account menu.
      await s.goto(base + "/?settings=rules");
      await s.getByRole("heading", { name: "Rules" }).waitFor();
      await shot(s, `settings-rules-${tag}`);
      if (rules[0]) {
        await s.goto(
          base + `/?settings=rules&rule=${rules.find((r) => r.match?.all)?.id ?? rules[0].id}`,
        );
        await s.getByRole("button", { name: "Count past mail…" }).click();
        await s.getByRole("button", { name: /^Apply to / }).waitFor();
        await shot(s, `settings-rule-${tag}`);
      }

      // The review tier: held mail and its recall (the second press is
      // shown, never made), a draft the checks escalated, the preview.
      if (held) {
        await s.goto(base + "/?area=outbox&st=held");
        await s.getByRole("list", { name: "Outbox" }).waitFor();
        await shot(s, `outbox-held-${tag}`);
        await s.goto(base + `/?area=outbox&st=held&o=${held}`);
        await s.getByRole("button", { name: "Recall…" }).click();
        await s.getByRole("button", { name: "Recall it" }).waitFor();
        await shot(s, `outbox-held-recall-${tag}`);
      }
      if (escalated) {
        await s.goto(base + `/?area=drafts&d=${escalated}`);
        await s.getByRole("list", { name: "Checks that escalated it" }).waitFor();
        await shot(s, `drafts-escalated-${tag}`);
      }
      if (reviewFrom) {
        await s.goto(base + "/?compose=1");
        await s.getByLabel("From", { exact: true }).selectOption(reviewFrom);
        await s.getByText("Send runs the review").waitFor();
        await shot(s, `compose-review-${tag}`);
      }
      // Notes beside the mail, and writing one's own (§8.10).
      if (noted) {
        await s.goto(base + `/?m=${noted}`);
        await s.getByRole("region", { name: "Notes" }).waitFor();
        await shot(s, `reading-notes-${tag}`);
        if (await s.getByRole("button", { name: "Add a note" }).count()) {
          await s.getByRole("button", { name: "Add a note" }).click();
          await s.getByRole("textbox", { name: "Note" }).waitFor();
          await shot(s, `reading-note-editor-${tag}`);
        }
      }
      // Whom a claimed prefix was given to, edited where its mail is (§12.12).
      if (claimed) {
        await s.goto(base + `/?prefix=${encodeURIComponent(claimed)}`);
        await s
          .getByRole("region", { name: "Prefix facts" })
          .getByRole("button", { name: /^(Edit|Set expected domains)$/ })
          .click();
        await s.getByLabel("Given to").waitFor();
        await shot(s, `prefix-expect-${tag}`);
      }
      // Who may do what: look and tighten; the second press is shown, never made.
      if (admin) {
        for (const section of [
          "credentials",
          "identities",
          "grants",
          "accounts",
          "transports",
          "channels",
          "notifications",
        ]) {
          await s.goto(base + `/?settings=${section}`);
          await s.locator("main h2").waitFor();
          await shot(s, `settings-${section}-${tag}`);
        }
        // The form a mailbox is added with; nothing is submitted.
        await s.goto(base + "/?settings=accounts");
        await s.getByRole("button", { name: "Add mailbox" }).click();
        await s.getByText("Add a mailbox to pull").waitFor();
        await shot(s, `settings-accounts-add-${tag}`);
        if (await s.getByRole("button", { name: "Tighten…" }).count()) {
          await s.getByRole("button", { name: "Tighten…" }).first().click();
          await s.getByRole("radio").first().check();
          await s.getByRole("button", { name: "Tighten", exact: true }).click();
          await s.getByRole("button", { name: "Tighten it" }).waitFor();
          await shot(s, `settings-grant-tighten-${tag}`);
        }
      }

      // A scorer's estimates in their own voice (§12.5, §15.4): the leanings
      // view and its marks, the estimate beside the mail, folded and open,
      // search narrowed by a leaning, and the report.
      if (triage) {
        await s.goto(base + "/?area=leanings");
        await s.getByRole("heading", { name: "Model leans to…" }).waitFor();
        await shot(s, `area-leanings-${tag}`);
        if (leaning) {
          await s.goto(base + `/?area=leanings&m=${leaning.id}`);
          await s.getByRole("region", { name: "A model's estimate" }).waitFor();
          await shot(s, `reading-estimate-${tag}`);
          await s
            .getByRole("button", { name: "Every probability, and what the model was given" })
            .click();
          await s.getByRole("button", { name: "Fold the estimate into one line" }).waitFor();
          await shot(s, `reading-estimate-open-${tag}`);
          const word = leaning.subject.split(/\s+/).find((w) => w.length > 3) ?? leaning.subject;
          await s.goto(base + `/?q=${encodeURIComponent(`triage:phishing:yes ${word}`)}`);
          await s.getByRole("listbox", { name: "Messages" }).waitFor();
          await shot(s, `search-leaning-${tag}`);
        }
        // Searching from the leanings view starts from its leaning, in the box.
        await s.goto(base + "/?area=leanings");
        await s.getByRole("heading", { name: "Model leans to…" }).waitFor();
        await s.getByRole("combobox", { name: "Search mail" }).focus();
        await s.keyboard.type("triage:");
        await s.getByRole("listbox", { name: "Values for triage:" }).waitFor();
        await shot(s, `search-complete-triage-${tag}`);
        await s.goto(base + "/?settings=triage");
        await s.getByRole("region", { name: "Estimate report" }).waitFor();
        await shot(s, `settings-triage-${tag}`);
      }

      // The account menu: who is signed in, the language, signing out.
      await s.goto(base + "/");
      await s.getByRole("button", { name: "Account" }).click();
      await s.getByRole("menu").waitFor();
      await shot(s, `account-menu-${tag}`);
      await s.close();
    }
  }

  for (const lang of translations) {
    for (const scheme of ["light", "dark"]) {
      for (const size of Object.keys(sizes)) {
        const s = await page(scheme, size, state, lang);
        const tag = `${lang}-${scheme}-${size}`;
        const t = catalogues[lang];
        for (const area of ["conversations", "notifications"]) {
          await s.goto(base + `/?area=${area}`);
          await s.getByRole("listbox").waitFor();
          await shot(s, `list-${area}-${tag}`);
        }
        for (const [name, id] of Object.entries({
          reading: lunch,
          "reading-otp": otp,
          "reading-attachments": invoice,
        })) {
          if (!id) continue;
          await s.goto(base + `/?m=${id}`);
          await s.locator("article h1").waitFor();
          await shot(s, `${name}-${tag}`);
        }
        await s.goto(base + "/?compose=1");
        await s.locator("textarea").waitFor();
        await shot(s, `compose-${tag}`);
        await s.goto(base + "/");
        await s.getByRole("button", { name: t.search_help }).click();
        await s.getByRole("dialog", { name: t.search_help }).waitFor();
        await shot(s, `search-help-${tag}`);
        await s.goto(base + "/?q=" + encodeURIComponent("FROM:example form:alice after:-7d"));
        await s.getByText(t.search_read_as).waitFor();
        await shot(s, `search-echo-${tag}`);
        await s.goto(base + "/?q=" + encodeURIComponent("invoice after:2026-13-01"));
        await s.getByRole("alert").waitFor();
        await shot(s, `search-error-${tag}`);
        await s.goto(base + "/?area=leaks");
        await s.locator("section h1").first().waitFor();
        await shot(s, `area-leaks-${tag}`);
        // The verdicts and their warnings, said by the UI in its own language.
        if (flagged) {
          await s.goto(base + `/?prefix=${encodeURIComponent(flagged)}`);
          await s.getByRole("list", { name: t.leak_offending_label }).waitFor();
          await shot(s, `prefix-leaks-${tag}`);
          for (const [verdict, id] of Object.entries(offending)) {
            if (!id) continue;
            await s.goto(base + `/?prefix=${encodeURIComponent(flagged)}&m=${id}`);
            await s.getByRole("article").getByText(t.facts_prefix, { exact: true }).waitFor();
            await shot(s, `reading-${verdict}-${tag}`);
          }
        }
        await s.goto(base + "/?area=proposals");
        await s.locator("section h1").first().waitFor();
        await shot(s, `area-proposals-${tag}`);
        if (labelled) {
          await s.goto(base + `/?m=${labelled.id}`);
          await s.getByRole("button", { name: t.annotate_why }).click();
          await s
            .getByRole("region", { name: t.history_label })
            .getByRole("listitem")
            .first()
            .waitFor();
          await shot(s, `reading-history-${tag}`);
        }
        if (lunch) {
          await s.goto(base + `/?m=${lunch}`);
          await s.getByRole("button", { name: t.annotate_like_this }).click();
          await s
            .getByRole("region", { name: t.drafter_label })
            .getByRole("checkbox")
            .first()
            .waitFor();
          await s.waitForLoadState("networkidle");
          await shot(s, `reading-drafter-${tag}`);
        }
        if (ruleProposal) {
          await s.goto(base + `/?area=proposals&p=${ruleProposal.id}&m=${ruleProposal.message_id}`);
          await s.getByRole("button", { name: t.proposal_review_rule }).click();
          await s.getByRole("region", { name: t.rule_preview_label }).waitFor();
          await s.waitForLoadState("networkidle");
          await shot(s, `proposal-rule-${tag}`);
        }
        if (held) {
          await s.goto(base + `/?area=outbox&st=held&o=${held}`);
          await s.getByRole("button", { name: t.review_recall }).click();
          await s.getByRole("button", { name: t.review_recall_confirm }).waitFor();
          await shot(s, `outbox-held-recall-${tag}`);
        }
        if (escalated) {
          await s.goto(base + `/?area=drafts&d=${escalated}`);
          await s.getByRole("list", { name: t.review_escalated_label }).waitFor();
          await shot(s, `drafts-escalated-${tag}`);
        }
        if (reviewFrom) {
          await s.goto(base + "/?compose=1");
          await s.getByLabel(t.send_field_from, { exact: true }).selectOption(reviewFrom);
          await s.getByText(t.send_reviews_it).waitFor();
          await shot(s, `compose-review-${tag}`);
        }
        if (noted) {
          await s.goto(base + `/?m=${noted}`);
          await s.getByRole("region", { name: t.notes_label }).waitFor();
          await shot(s, `reading-notes-${tag}`);
        }
        if (claimed) {
          await s.goto(base + `/?prefix=${encodeURIComponent(claimed)}`);
          await s.getByRole("region", { name: t.leak_facts_label }).waitFor();
          await s.waitForLoadState("networkidle");
          await shot(s, `prefix-facts-${tag}`);
        }
        if (admin) {
          for (const section of [
            "credentials",
            "identities",
            "grants",
            "accounts",
            "transports",
            "channels",
            "notifications",
          ]) {
            await s.goto(base + `/?settings=${section}`);
            await s.locator("main h2").waitFor();
            await shot(s, `settings-${section}-${tag}`);
          }
          if (await s.getByRole("button", { name: t.grants_tighten }).count()) {
            await s.getByRole("button", { name: t.grants_tighten }).first().click();
            await s.getByRole("radio").first().check();
            await s.getByRole("button", { name: t.grants_tighten_apply, exact: true }).click();
            await s.getByRole("button", { name: t.grants_tighten_confirm }).waitFor();
            await shot(s, `settings-grant-tighten-${tag}`);
          }
        }
        if (triage) {
          await s.goto(base + "/?area=leanings");
          await s.getByRole("heading", { name: t.area_leanings }).waitFor();
          await shot(s, `area-leanings-${tag}`);
          if (leaning) {
            await s.goto(base + `/?area=leanings&m=${leaning.id}`);
            await s.getByRole("button", { name: t.triage_unfold }).click();
            await s.getByRole("button", { name: t.triage_fold }).waitFor();
            await shot(s, `reading-estimate-open-${tag}`);
          }
          await s.goto(base + "/?settings=triage");
          await s.getByRole("region", { name: t.settings_triage }).waitFor();
          await shot(s, `settings-triage-${tag}`);
        }
        await s.goto(base + "/?settings=rules");
        await s.locator("main h2").waitFor();
        await shot(s, `settings-rules-${tag}`);
        if (rules[0]) {
          await s.goto(base + `/?settings=rules&rule=${rules[0].id}`);
          await s.locator("main h2").waitFor();
          await s.waitForLoadState("networkidle");
          await shot(s, `settings-rule-${tag}`);
        }
        await s.close();
      }
    }
  }
}

await browser.close();
if (problems.length) {
  console.error("console errors:\n" + problems.join("\n"));
  process.exitCode = 1;
}
