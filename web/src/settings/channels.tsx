import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import type { ManagedChannel } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { withElevation } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import { Dates, Field, inputClass, SectionHeader, WideningForm } from "./section";
import { LockedNote, SecretLine } from "./secret-state";

// Notification channels (§8.4, §17): a Telegram bot each, its token
// sealed. Adding one or giving it a new token writes a secret, so it asks
// for the passkey first; the token field is write-only. Removing one
// erases the token and revokes every binding on it, a tightening.
// Chats are bound in "Notifications", each person their own.

const touched = ["/api/v1/admin/channels", "/api/v1/admin/secrets", "/api/v1/bindings"];

export function ChannelsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/channels", {});
  const [adding, setAdding] = useState(false);
  const [removed, setRemoved] = useState(false);
  const all = q.data?.channels ?? [];
  const shown = removed ? all : all.filter((c) => !c.disabled_at);
  const locked = !!q.data?.locked;
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_channels()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={adding}
          disabled={!q.data || locked}
          onClick={() => setAdding((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.channels_add()}
        </Button>
      </SectionHeader>
      <p className="text-small text-muted-foreground">{m.channels_hint()}</p>
      {locked && <LockedNote text={m.channels_locked()} />}
      {adding && <TokenForm onDone={() => setAdding(false)} />}
      <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
        <input
          type="checkbox"
          className="accent-current"
          checked={removed}
          onChange={(e) => setRemoved(e.target.checked)}
        />
        {m.channels_show_removed({ n: all.length - all.filter((c) => !c.disabled_at).length })}
      </label>
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground">{m.channels_none()}</p>
      ) : (
        <ul aria-label={m.settings_channels()} className="flex flex-col border-t">
          {shown.map((c) => (
            <ChannelRow key={c.id} c={c} locked={locked} />
          ))}
        </ul>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.channels_on_host()}
          command="yuuka channel add NAME --token-file -"
        />
      </details>
    </div>
  );
}

function bot(c: ManagedChannel): string {
  return c.bot_username ? `@${c.bot_username} (${c.bot_id})` : c.bot_id;
}

function ChannelRow({ c, locked }: { c: ManagedChannel; locked: boolean }) {
  const qc = useQueryClient();
  const [token, setToken] = useState(false);
  const [sent, setSent] = useState<{ n: number; error?: string }>();
  const [err, setErr] = useState<unknown>();
  const off = !!c.disabled_at;
  const test = async () => {
    setErr(undefined);
    setSent(undefined);
    try {
      const r = await data(
        api.POST("/api/v1/admin/channels/{name}/test", { params: { path: { name: c.name } } }),
      );
      setSent({ n: r.sent, error: r.error });
    } catch (e) {
      setErr(e);
    }
  };
  const remove = async () => {
    await data(
      api.POST("/api/v1/admin/channels/{name}/disable", {
        params: { path: { name: c.name } },
        body: { revision: c.revision },
      }),
    );
    for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
  };
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{c.name}</span>
        <span className="font-mono text-small">{bot(c)}</span>
        {off && (
          <span className="text-meta">
            {m.channels_removed({ when: listTime(c.disabled_at ?? "") })}
          </span>
        )}
      </p>
      <Dates
        items={[
          [m.channels_bindings(), off ? undefined : String(c.bindings)],
          [m.credentials_created(), listTime(c.created_at)],
        ]}
      />
      {c.secret && <SecretLine label={m.channels_token()} s={c.secret} />}
      {token ? (
        <TokenForm c={c} onDone={() => setToken(false)} />
      ) : (
        <div className="flex flex-wrap items-start gap-2 pt-0.5">
          <Button size="xs" variant="outline" disabled={locked} onClick={() => setToken(true)}>
            {off ? m.channels_add_again() : m.channels_new_token()}
          </Button>
          {!off && (
            <Button
              size="xs"
              variant="outline"
              disabled={locked || c.bindings === 0}
              onClick={() => void test()}
            >
              {m.channels_test()}
            </Button>
          )}
          {!off && (
            <ConfirmButton
              label={m.channels_remove()}
              confirmLabel={m.channels_remove_confirm()}
              cost={m.channels_remove_cost({ name: c.name, n: c.bindings })}
              onConfirm={remove}
            />
          )}
        </div>
      )}
      {sent && (
        <p role="status" className="text-small text-system-foreground">
          {m.channels_test_sent({ n: sent.n })}{" "}
          {sent.error && <span className="font-mono text-warning">{sent.error}</span>}
        </p>
      )}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </li>
  );
}

/**
 * A bot token for channel c (a new one when absent): sent once, sealed
 * for the bot Telegram names, never shown again.
 */
function TokenForm({ c, onDone }: { c?: ManagedChannel; onDone: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(c?.name ?? "");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const ready =
    /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(name) && /^\d+:\S{20,}$/.test(token.trim());
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await withElevation(() =>
        data(
          api.PUT("/api/v1/admin/channels/{name}", {
            params: { path: { name } },
            body: { revision: c?.revision ?? 0, token: token.trim() },
          }),
        ),
      );
      setToken("");
      for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
      onDone();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <WideningForm
      legend={
        !c
          ? m.channels_add_legend()
          : c.disabled_at
            ? m.channels_add_again_legend({ name: c.name })
            : m.channels_token_legend({ name: c.name })
      }
      onSubmit={() => void submit()}
    >
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        {!c && (
          <Field label={m.channels_name()} className="w-40">
            <input
              className={inputClass}
              value={name}
              placeholder="telegram"
              onChange={(e) => setName(e.target.value.trim())}
            />
          </Field>
        )}
        <Field label={m.channels_token()} className="w-80" hint={m.channels_token_hint()}>
          <input
            type="password"
            className={inputClass}
            value={token}
            autoComplete="off"
            onChange={(e) => setToken(e.target.value)}
          />
        </Field>
      </div>
      <p className="text-muted-foreground">
        {c && !c.disabled_at
          ? m.channels_token_cost({ bot: bot(c) })
          : c
            ? m.channels_add_again_cost()
            : m.channels_add_cost()}
      </p>
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={!ready || busy}>
          {c && !c.disabled_at ? m.channels_save_token() : m.channels_add_submit()}
        </Button>
        <Button type="button" size="xs" variant="ghost" onClick={onDone}>
          {m.rule_cancel()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </WideningForm>
  );
}
