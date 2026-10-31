import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, data } from "@/api/client";
import type { CountedAs } from "@/api/types";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

// Decisions about a prefix, made where its mail is read (§12.12, §13.10):
// claiming it, giving it back, and whom it was given to. They change no
// one's permissions, so they need only their scope (prefix:manage), and
// they go through the same store functions as `yuuka prefix`.

/** After a prefix decision: the prefix lists and every list of its mail. */
export function invalidatePrefixes(qc: QueryClient) {
  const stale = ["/api/v1/prefixes", "/api/v1/messages", "/api/v1/search", "/api/v1/threads"];
  void qc.invalidateQueries({
    predicate: (q) =>
      typeof q.queryKey[1] === "string" &&
      stale.some((p) => (q.queryKey[1] as string).startsWith(p)),
  });
}

/** Claim: its mail stops being downgraded unless DMARC failed, and new mail to it notifies (§3.3). */
export function ClaimButton({ address, size = "xs" }: { address: string; size?: "xs" | "sm" }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  return (
    <span className="inline-flex items-center gap-2">
      <Button
        size={size}
        variant="outline"
        disabled={busy}
        aria-label={m.prefix_claim_label({ address })}
        onClick={async (e) => {
          e.stopPropagation();
          setBusy(true);
          setErr(undefined);
          try {
            await data(
              api.POST("/api/v1/prefixes/{address}/claim", { params: { path: { address } } }),
            );
            invalidatePrefixes(qc);
          } catch (x) {
            setErr(x);
          } finally {
            setBusy(false);
          }
        }}
      >
        {m.prefix_claim()}
      </Button>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </span>
  );
}

/** Unclaim, with its cost beside the second press. */
export function UnclaimButton({ address }: { address: string }) {
  const qc = useQueryClient();
  return (
    <ConfirmButton
      label={m.prefix_unclaim()}
      confirmLabel={m.prefix_unclaim_confirm()}
      cost={m.prefix_unclaim_cost()}
      onConfirm={async () => {
        await data(
          api.POST("/api/v1/prefixes/{address}/unclaim", { params: { path: { address } } }),
        );
        invalidatePrefixes(qc);
      }}
    />
  );
}

/**
 * The domains a claimed prefix was given to, edited in place. Saving
 * answers with what the leak check now makes of its mail; a subdomain is
 * compared by its organizational domain, which yuuka says once.
 */
export function ExpectEditor({
  address,
  expected,
  onClose,
}: {
  address: string;
  expected: string[];
  onClose: (counted?: CountedAs[]) => void;
}) {
  const qc = useQueryClient();
  const [text, setText] = useState(expected.join(", "));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const save = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      const domains = text
        .split(/[\s,;]+/)
        .map((d) => d.trim())
        .filter(Boolean);
      const r = await data(
        api.PUT("/api/v1/prefixes/{address}/expected-domains", {
          params: { path: { address } },
          body: { domains },
        }),
      );
      invalidatePrefixes(qc);
      onClose(r.counted_as);
    } catch (x) {
      setErr(x);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="flex flex-col gap-1"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <label className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground">{m.prefix_expect_label()}</span>
        <input
          // oxlint-disable-next-line jsx-a11y/no-autofocus -- opened by a press to type here
          autoFocus
          value={text}
          spellCheck={false}
          placeholder={m.prefix_expect_placeholder()}
          onChange={(e) => setText(e.target.value)}
          className="h-6 min-w-0 flex-1 rounded-md border border-input bg-background px-1.5 font-mono text-small outline-none focus-visible:border-ring"
        />
      </label>
      <span className="flex flex-wrap items-center gap-2">
        <Button size="xs" type="submit" disabled={busy}>
          {m.prefix_expect_save()}
        </Button>
        <Button size="xs" variant="ghost" type="button" onClick={() => onClose()}>
          {m.rule_cancel()}
        </Button>
        <span className="text-muted-foreground">{m.prefix_expect_hint()}</span>
      </span>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </form>
  );
}

/** "mail.example.com counts as example.com": how the leak check reads what was typed. */
export function CountedAsNotes({ counted }: { counted: CountedAs[] }) {
  if (counted.length === 0) return null;
  return (
    <ul className="flex flex-col text-muted-foreground">
      {counted.map((c) => (
        <li key={c.domain}>{m.prefix_counted_as({ domain: c.domain, as: c.as })}</li>
      ))}
    </ul>
  );
}
