import { useQueryClient } from "@tanstack/react-query";
import { useId, useState } from "react";

import { $api, api, ApiError, data } from "@/api/client";
import type { ManagedGrant } from "@/api/types";
import { CommandLine, HostCommand, hostCommandOf } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { duration, principal, problem, resumeCommand, scopeText, tierLabel } from "@/send/outgoing";

import {
  grantParts,
  grantSummary,
  lowerLimit,
  removeCommand,
  subsetOf,
  tightenBody,
  tightenings,
  tighteningCost,
  type Tightening,
} from "./authority";
import { SectionHeader } from "./section";

// Grants (§13.5, §13.10): who may send as which identity, and how far.
// The page shows what applies now (after defaults and the owner cap) and
// offers only what is tighter; raising and adding are done on the host,
// and the page names the command instead of a greyed-out button.

const grantsKey = ["get", "/api/v1/admin/grants"];

export function GrantsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/grants", {});
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_grants()} />
      <HostCommand
        why={m.grants_on_host()}
        command="yuuka grant set --identity ADDRESS --principal KIND:NAME --policy TIER"
      />
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : q.data.grants.length === 0 ? (
        <p className="text-muted-foreground">{m.grants_none()}</p>
      ) : (
        <ul aria-label={m.settings_grants()} className="flex flex-col border-t">
          {q.data.grants.map((g) => (
            <GrantRow key={`${g.identity.id}:${g.principal.id}`} g={g} />
          ))}
        </ul>
      )}
    </div>
  );
}

const key = (g: ManagedGrant) => ({ identity: g.identity.id, principal: g.principal.id });

function GrantRow({ g }: { g: ManagedGrant }) {
  const qc = useQueryClient();
  const [tightening, setTightening] = useState(false);
  const a = g.applied;
  const parts = grantParts(a);
  const off = g.identity.disabled;
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{g.identity.address}</span>
        <span className="text-muted-foreground">→</span>
        <span className="font-mono">{principal(g.principal)}</span>
        {a.suspended && (
          <span className="text-meta font-medium text-warning">{m.grants_suspended()}</span>
        )}
        {off && <span className="text-meta">{m.grants_identity_disabled()}</span>}
      </p>
      <p className="flex flex-wrap items-baseline gap-x-2 text-small">
        <Signature speaker={system} />
        <span>{grantSummary(a)}</span>
        {parts.dormant && (
          <span className="text-muted-foreground">
            {m.grants_dormant({ rest: parts.rest.join(" · ") })}
          </span>
        )}
      </p>
      {g.owner_capped && (
        <p className="text-small text-muted-foreground">
          {m.grants_owner_capped({
            owner: principal(g.identity.owner),
            stored: tierLabel(g.stored.send_policy),
          })}
        </p>
      )}
      {a.suspended && (
        // Resuming lets mail through again: the host's to do (§13.10).
        <HostCommand
          className="border-warning/60"
          why={m.grants_suspended_why()}
          command={resumeCommand(g.identity.address, principal(g.principal))}
        />
      )}
      <p className="text-small text-muted-foreground">
        {m.grants_default({ summary: grantSummary(g.default) })}
      </p>
      {!off && (
        <div className="flex flex-wrap items-start gap-2 pt-0.5">
          <Button
            size="xs"
            variant="outline"
            aria-expanded={tightening}
            onClick={() => setTightening((t) => !t)}
          >
            {m.grants_tighten()}
          </Button>
          <Remove g={g} />
        </div>
      )}
      {off && <Remove g={g} />}
      {tightening && (
        <Tighten
          g={g}
          onDone={() => {
            setTightening(false);
            void qc.invalidateQueries({ queryKey: grantsKey });
          }}
        />
      )}
    </li>
  );
}

/**
 * Remove a grant: offered only when the default that applies afterwards
 * lets nothing more through (removal_widens empty); otherwise the host
 * command, with what would widen.
 */
function Remove({ g }: { g: ManagedGrant }) {
  const qc = useQueryClient();
  const [host, setHost] = useState<{ why: string; command: string }>();
  if (g.removal_widens.length > 0) {
    return (
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.grants_remove_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.grants_remove_widens({ fields: g.removal_widens.join(", ") })}
          command={removeCommand(g)}
        />
      </details>
    );
  }
  return (
    <span className="flex flex-col gap-1">
      <ConfirmButton
        label={m.grants_remove()}
        confirmLabel={m.grants_remove_confirm()}
        cost={m.grants_remove_cost({ summary: grantSummary(g.default) })}
        onConfirm={async () => {
          await api.DELETE("/api/v1/admin/grants/{identity}/{principal}", {
            params: { path: key(g) },
          });
          void qc.invalidateQueries({ queryKey: grantsKey });
        }}
        onError={(e) => {
          const h = widening(e);
          if (h) setHost(h);
          return !!h;
        }}
      />
      {host && <WideningHint host={host} />}
    </span>
  );
}

/** A refusal that names the host command (403 widening_requires_cli), split for showing. */
function widening(e: unknown): { why: string; command: string } | undefined {
  if (!(e instanceof ApiError) || e.code !== "widening_requires_cli") return undefined;
  return hostCommandOf(e.message) ?? { why: e.message, command: "" };
}

function WideningHint({ host }: { host: { why: string; command: string } }) {
  return (
    <div className="flex flex-col gap-1 rounded-md border border-system-border bg-system-surface px-2 py-1.5 text-small">
      <p className="flex flex-wrap items-baseline gap-x-2">
        <Signature speaker={system} />
        <span className="font-medium">{m.grants_widening()}</span>
        <span className="text-muted-foreground">{host.why}</span>
      </p>
      {host.command && <CommandLine command={host.command} />}
    </div>
  );
}

const optionKey = (t: Tightening) =>
  t.kind === "tier"
    ? `tier:${t.tier}`
    : t.kind === "scope"
      ? `scope:${t.scope}`
      : t.kind === "limit"
        ? `limit:${t.field}`
        : t.kind;

/**
 * The changes that only tighten this grant, one at a time: pick one (with
 * its value), then press Tighten and press again beside its cost.
 */
function Tighten({ g, onDone }: { g: ManagedGrant; onDone: () => void }) {
  const a = g.applied;
  const options = tightenings(a);
  const id = useId();
  const [pick, setPick] = useState<string>();
  const [limits, setLimits] = useState<Record<string, string>>({});
  const holdNow = Math.round(a.hold_seconds / 60);
  const [hold, setHold] = useState(String(Math.max(holdNow * 2, holdNow + 10)));
  const [keep, setKeep] = useState<string[]>(a.allowlist);
  const [host, setHost] = useState<{ why: string; command: string }>();
  const chosen = options.find((t) => optionKey(t) === pick);
  const value = (t: Tightening) => ({
    limit: t.kind === "limit" ? Number(limits[t.field] ?? "") : undefined,
    holdMinutes: t.kind === "hold" ? Number(hold) : undefined,
    keep,
  });
  const valid = (t: Tightening) => {
    switch (t.kind) {
      case "limit":
        return (
          (limits[t.field] ?? "") !== "" && lowerLimit(a.limits[t.field], Number(limits[t.field]))
        );
      case "hold":
        return Number.isInteger(Number(hold)) && Number(hold) * 60 > a.hold_seconds;
      case "allowlist":
        return subsetOf(a.allowlist, keep);
      default:
        return true;
    }
  };
  if (options.length === 0)
    return <p className="text-small text-muted-foreground">{m.grants_nothing_tighter()}</p>;
  return (
    <fieldset className="flex flex-col gap-1.5 rounded-md border px-2 py-2 text-small">
      <legend className="px-1 text-muted-foreground">{m.grants_tighten_legend()}</legend>
      {options.map((t) => {
        const k = optionKey(t);
        return (
          // A div, not a label: some options hold inputs of their own.
          <div
            key={k}
            className={cn(
              "flex items-start gap-2 rounded-md border px-2 py-1.5 hover:bg-muted",
              pick === k && "border-foreground/40 bg-accent",
            )}
          >
            <input
              id={`${id}-${k}`}
              type="radio"
              name={id}
              className="mt-0.5 accent-current"
              checked={pick === k}
              onChange={() => setPick(k)}
            />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="flex flex-wrap items-center gap-x-1.5 gap-y-1">
                <label htmlFor={`${id}-${k}`} className="cursor-pointer font-medium">
                  {optionName(t, g)}
                </label>
                <OptionValue
                  t={t}
                  g={g}
                  limit={t.kind === "limit" ? (limits[t.field] ?? "") : ""}
                  onLimit={(v) => t.kind === "limit" && setLimits((l) => ({ ...l, [t.field]: v }))}
                  hold={hold}
                  onHold={setHold}
                  keep={keep}
                  onKeep={setKeep}
                  onFocus={() => setPick(k)}
                />
              </span>
              <span className="text-muted-foreground">{tighteningCost(g, t, value(t))}</span>
            </span>
          </div>
        );
      })}
      <div className="pt-1">
        <ConfirmButton
          label={m.grants_tighten_apply()}
          confirmLabel={m.grants_tighten_confirm()}
          cost={chosen ? tighteningCost(g, chosen, value(chosen)) : ""}
          disabled={!chosen || !valid(chosen)}
          onConfirm={async () => {
            if (!chosen) return;
            setHost(undefined);
            await data(
              api.POST("/api/v1/admin/grants/{identity}/{principal}/tighten", {
                params: { path: key(g) },
                body: tightenBody(chosen, value(chosen)),
              }),
            );
            onDone();
          }}
          onError={(e) => {
            const h = widening(e);
            if (h) setHost(h);
            return !!h;
          }}
        />
      </div>
      {host && <WideningHint host={host} />}
    </fieldset>
  );
}

const numberInput =
  "h-6 w-16 rounded-md border border-input bg-background px-1.5 font-mono text-small tabular-nums outline-none focus-visible:border-ring";

const limitWords = {
  max_per_hour: m.grants_opt_hour,
  max_per_day: m.grants_opt_day,
  max_new_recipients_per_day: m.grants_opt_new,
};

/** The words of one option, the radio's label. */
function optionName(t: Tightening, g: ManagedGrant): string {
  switch (t.kind) {
    case "suspend":
      return m.grants_opt_suspend();
    case "tier":
      return m.grants_opt_tier({ tier: tierLabel(t.tier) });
    case "scope":
      return m.grants_opt_scope({ scope: scopeText(t.scope, g.applied.allowlist) });
    case "allowlist":
      return m.grants_opt_allowlist();
    case "limit":
      return limitWords[t.field]();
    case "hold":
      return m.grants_opt_hold();
  }
}

/** The value an option takes, beside its name: a number, or the entries to keep. */
function OptionValue({
  t,
  g,
  limit,
  onLimit,
  hold,
  onHold,
  keep,
  onKeep,
  onFocus,
}: {
  t: Tightening;
  g: ManagedGrant;
  limit: string;
  onLimit: (v: string) => void;
  hold: string;
  onHold: (v: string) => void;
  keep: string[];
  onKeep: (k: string[]) => void;
  onFocus: () => void;
}) {
  const a = g.applied;
  switch (t.kind) {
    case "allowlist":
      return a.allowlist.map((x) => (
        <label key={x} className="inline-flex items-center gap-1 font-mono">
          <input
            type="checkbox"
            className="accent-current"
            checked={keep.includes(x)}
            onFocus={onFocus}
            onChange={(e) => {
              onFocus();
              onKeep(e.target.checked ? [...keep, x] : keep.filter((y) => y !== x));
            }}
          />
          {x}
        </label>
      ));
    case "limit": {
      const cur = a.limits[t.field];
      return (
        <>
          <input
            type="number"
            min={0}
            max={cur === undefined ? undefined : cur - 1}
            aria-label={limitWords[t.field]()}
            className={numberInput}
            value={limit}
            onFocus={onFocus}
            onChange={(e) => onLimit(e.target.value)}
          />
          <span className="text-muted-foreground">
            {cur === undefined ? m.grants_now_unlimited() : m.grants_now({ n: cur })}
          </span>
        </>
      );
    }
    case "hold":
      return (
        <>
          <input
            type="number"
            min={Math.floor(a.hold_seconds / 60) + 1}
            aria-label={m.grants_opt_hold()}
            className={numberInput}
            value={hold}
            onFocus={onFocus}
            onChange={(e) => onHold(e.target.value)}
          />
          <span className="text-muted-foreground">
            {m.grants_minutes_now({ now: duration(a.hold_seconds) })}
          </span>
        </>
      );
    default:
      return null;
  }
}
