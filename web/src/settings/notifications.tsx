import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import type { BindCodeView, BindStart, BindingList, Event, MyBinding } from "@/api/types";
import { CommandLine, HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { listTime } from "@/lib/format";
import { useNow } from "@/lib/now";
import { cn } from "@/lib/utils";
import { withElevation } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import { Dates, Field, inputClass, SectionHeader, selectClass, WideningForm } from "./section";
import { LockedNote } from "./secret-state";
import { allEvents, chatKind, eventLabel } from "./thirdparty";

// The viewer's own notifications (§8.4, §13.7): the Telegram chats bound
// to them, what each receives, and binding another. Binding is two-sided:
// yuuka hands out a code, the chat sends it to the bot, Telegram reports
// who sent it, and only then does the person here confirm that this is
// their chat. Only one's own chats are bound here; for someone else it is
// done on the host. Revoking and changing what a chat receives tighten.

const bindingsKey = ["get", "/api/v1/bindings"];

export function NotificationsSection() {
  const q = $api.useQuery("get", "/api/v1/bindings", {});
  const [binding, setBinding] = useState(false);
  const list = q.data;
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_notifications()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={binding}
          disabled={!list || list.locked || list.channels.length === 0}
          onClick={() => setBinding((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.bind_start()}
        </Button>
      </SectionHeader>
      <p className="text-small text-muted-foreground">{m.notifications_hint()}</p>
      {list?.locked && <LockedNote text={m.notifications_locked()} />}
      {list && !list.locked && list.channels.length === 0 && (
        <p className="text-small text-muted-foreground">{m.notifications_no_channel()}</p>
      )}
      {binding && list && <BindFlow list={list} onDone={() => setBinding(false)} />}
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !list ? (
        <Skeleton className="h-24 w-full" />
      ) : list.bindings.length === 0 ? (
        <p className="text-muted-foreground">{m.notifications_none()}</p>
      ) : (
        <ul aria-label={m.settings_notifications()} className="flex flex-col border-t">
          {list.bindings.map((b) => (
            <BindingRow key={b.id} b={b} />
          ))}
        </ul>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.notifications_on_host()}
          command="yuuka channel bind CHANNEL --principal human:NAME"
        />
      </details>
    </div>
  );
}

function BindingRow({ b }: { b: MyBinding }) {
  const qc = useQueryClient();
  const [err, setErr] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const setEvents = async (events: Event[]) => {
    setBusy(true);
    setErr(undefined);
    try {
      await data(
        api.PUT("/api/v1/bindings/{id}/events", {
          params: { path: { id: b.id } },
          body: { events },
        }),
      );
      void qc.invalidateQueries({ queryKey: bindingsKey });
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const revoke = async () => {
    await data(api.POST("/api/v1/bindings/{id}/revoke", { params: { path: { id: b.id } } }));
    void qc.invalidateQueries({ queryKey: bindingsKey });
    void qc.invalidateQueries({ queryKey: ["get", "/api/v1/admin/channels"] });
  };
  return (
    <li className="flex flex-col gap-1 border-b px-2 py-2">
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="min-w-0 truncate font-medium">{b.label || b.chat}</span>
        <span className="font-mono text-small text-muted-foreground">
          {b.bot_username ? `@${b.bot_username}` : b.channel}
        </span>
      </p>
      <Dates
        items={[
          [m.bind_chat(), b.chat],
          [
            m.bind_buttons(),
            b.button_user ? m.bind_buttons_user({ id: b.button_user }) : m.bind_no_buttons(),
          ],
          [m.credentials_created(), listTime(b.created_at)],
          [m.credentials_last_used(), b.last_used_at ? listTime(b.last_used_at) : undefined],
        ]}
      />
      <EventPicker value={b.events} disabled={busy} onChange={(e) => void setEvents(e)} />
      <div className="pt-0.5">
        <ConfirmButton
          label={m.bind_revoke()}
          confirmLabel={m.bind_revoke_confirm()}
          cost={m.bind_revoke_cost({ chat: b.label || b.chat })}
          onConfirm={revoke}
        />
      </div>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </li>
  );
}

/** What a chat receives; security alerts always, so they are not offered. */
function EventPicker({
  value,
  onChange,
  disabled,
}: {
  value: Event[];
  onChange: (e: Event[]) => void;
  disabled?: boolean;
}) {
  return (
    <fieldset className="flex flex-wrap items-center gap-x-3 gap-y-1 text-small">
      <legend className="sr-only">{m.bind_events()}</legend>
      <span className="text-muted-foreground">{m.bind_events()}</span>
      {allEvents.map((e) => (
        <label key={e} className="flex items-center gap-1">
          <input
            type="checkbox"
            className="accent-current"
            disabled={disabled}
            checked={value.includes(e)}
            onChange={(x) =>
              onChange(allEvents.filter((y) => (y === e ? x.target.checked : value.includes(y))))
            }
          />
          {eventLabel(e)}
        </label>
      ))}
      <span className="text-meta text-muted-foreground">{m.bind_security_always()}</span>
    </fieldset>
  );
}

/**
 * §13.7 for one's own chat: pick a channel, get a code (passkey first),
 * send it from the chat, see who Telegram says sent it, confirm or
 * decline.
 */
function BindFlow({ list, onDone }: { list: BindingList; onDone: () => void }) {
  const qc = useQueryClient();
  const [channel, setChannel] = useState(list.channels[0]?.name ?? "");
  const [start, setStart] = useState<BindStart>();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const begin = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      setStart(
        await withElevation(() => data(api.POST("/api/v1/bind-codes", { body: { channel } }))),
      );
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  if (start)
    return (
      <Pending
        start={start}
        onDone={() => {
          void qc.invalidateQueries({ queryKey: bindingsKey });
          void qc.invalidateQueries({ queryKey: ["get", "/api/v1/admin/channels"] });
          onDone();
        }}
        onAgain={() => setStart(undefined)}
      />
    );
  return (
    <WideningForm legend={m.bind_legend()} onSubmit={() => void begin()}>
      <p className="text-muted-foreground">{m.bind_how()}</p>
      <span className="flex flex-wrap items-end gap-2">
        {list.channels.length > 1 && (
          <Field label={m.bind_channel()}>
            <select
              className={selectClass}
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
            >
              {list.channels.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.name}
                  {c.bot_username ? ` (@${c.bot_username})` : ""}
                </option>
              ))}
            </select>
          </Field>
        )}
        <Button type="submit" size="xs" disabled={!channel || busy}>
          {m.bind_get_code()}
        </Button>
        <Button type="button" size="xs" variant="ghost" onClick={onDone}>
          {m.rule_cancel()}
        </Button>
      </span>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </WideningForm>
  );
}

/** The code handed out, until the bot hears it; then who sent it, to confirm or decline. */
function Pending({
  start,
  onDone,
  onAgain,
}: {
  start: BindStart;
  onDone: () => void;
  onAgain: () => void;
}) {
  const id = start.bind_code.id;
  const q = $api.useQuery(
    "get",
    "/api/v1/bind-codes/{id}",
    { params: { path: { id } } },
    {
      initialData: start.bind_code,
      refetchInterval: (query) => (query.state.data?.state === "waiting" ? 2000 : false),
    },
  );
  const code: BindCodeView = q.data ?? start.bind_code;
  // The expiry is decided on the server; this only stops asking after it.
  const now = useNow(1000);
  const left = Math.max(0, Math.round((Date.parse(code.expires_at) - now) / 1000));
  const expired = code.state === "expired" || left === 0;
  return (
    <section className="flex flex-col gap-2 rounded-md border border-system-border bg-system-surface px-3 py-2.5 text-small">
      <p className="flex flex-wrap items-baseline gap-x-2">
        <Signature speaker={system} />
        <span className="font-medium">{m.bind_pending_title({ channel: code.channel })}</span>
      </p>
      {code.state === "waiting" && !expired && (
        <>
          <SendCode start={start} left={left} />
          <span>
            <Button
              size="xs"
              variant="ghost"
              onClick={() =>
                void api
                  .POST("/api/v1/bind-codes/{id}/decline", { params: { path: { id } } })
                  .catch(() => undefined)
                  .then(onAgain)
              }
            >
              {m.rule_cancel()}
            </Button>
          </span>
        </>
      )}
      {code.state === "redeemed" && !expired && code.seen && (
        <Confirm code={code} onDone={onDone} onDecline={onDone} />
      )}
      {expired && code.state !== "confirmed" && (
        <p className="flex flex-wrap items-center gap-2">
          <span className="text-warning">{m.bind_expired()}</span>
          <Button size="xs" variant="outline" onClick={onAgain}>
            {m.bind_again()}
          </Button>
        </p>
      )}
    </section>
  );
}

function SendCode({ start, left }: { start: BindStart; left: number }) {
  const mins = Math.floor(left / 60);
  const secs = String(left % 60).padStart(2, "0");
  return (
    <div className="flex flex-col gap-1.5">
      {start.link ? (
        <>
          <span>{m.bind_open_link()}</span>
          <a
            href={start.link}
            target="_blank"
            rel="noopener noreferrer"
            className="self-start font-mono break-all text-system-foreground underline underline-offset-2"
          >
            {start.link}
          </a>
          {start.group_link && (
            <span className="text-muted-foreground">
              {m.bind_group_link()} <span className="font-mono break-all">{start.group_link}</span>
            </span>
          )}
        </>
      ) : (
        <>
          <span>{m.bind_send_command()}</span>
          <CommandLine command={`/start ${start.code}`} />
        </>
      )}
      <p className="flex flex-wrap items-center gap-x-2 text-muted-foreground" aria-live="polite">
        <span className="inline-block size-1.5 animate-pulse rounded-full bg-system" aria-hidden />
        {m.bind_waiting({ left: `${mins}:${secs}` })}
      </p>
    </div>
  );
}

function Confirm({
  code,
  onDone,
  onDecline,
}: {
  code: BindCodeView;
  onDone: () => void;
  onDecline: () => void;
}) {
  const seen = code.seen!;
  const [label, setLabel] = useState("");
  const [events, setEvents] = useState<Event[]>(allEvents);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const run = async (f: () => Promise<unknown>) => {
    setBusy(true);
    setErr(undefined);
    try {
      await f();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const confirm = () =>
    run(async () => {
      await withElevation(() =>
        data(
          api.POST("/api/v1/bind-codes/{id}/confirm", {
            params: { path: { id: code.id } },
            body: { label: label.trim() || undefined, events },
          }),
        ),
      );
      onDone();
    });
  const decline = () =>
    run(async () => {
      await api.POST("/api/v1/bind-codes/{id}/decline", { params: { path: { id: code.id } } });
      onDecline();
    });
  return (
    <div className="flex flex-col gap-2">
      <span>{m.bind_reported()}</span>
      {/* What Telegram reported. The name is whatever the sender chose: data, shown as text. */}
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 rounded-md border bg-background px-2 py-1.5">
        <dt className="text-muted-foreground">{m.bind_seen_name()}</dt>
        <dd className="min-w-0 font-medium break-words">{seen.label || "—"}</dd>
        <dt className="text-muted-foreground">{m.bind_seen_user()}</dt>
        <dd className="font-mono">{seen.user_id}</dd>
        <dt className="text-muted-foreground">{m.bind_seen_chat()}</dt>
        <dd className="font-mono">
          {seen.chat_id}{" "}
          <span className="font-sans text-muted-foreground">({chatKind(seen.chat_kind)})</span>
        </dd>
        <dt className="text-muted-foreground">{m.bind_seen_at()}</dt>
        <dd className="tabular-nums">{listTime(seen.at)}</dd>
      </dl>
      <p className={cn(seen.chat_kind === "private" ? "text-muted-foreground" : "text-warning")}>
        {seen.chat_kind === "private" ? m.bind_cost_private() : m.bind_cost_group()}
      </p>
      <Field label={m.bind_label()} className="w-64">
        <input
          className={cn(inputClass, "font-sans")}
          value={label}
          placeholder={seen.label}
          onChange={(e) => setLabel(e.target.value)}
        />
      </Field>
      <EventPicker value={events} onChange={setEvents} disabled={busy} />
      <span className="flex flex-wrap items-center gap-2">
        <Button size="xs" disabled={busy} onClick={() => void confirm()}>
          {m.bind_confirm()}
        </Button>
        <Button size="xs" variant="outline" disabled={busy} onClick={() => void decline()}>
          {m.bind_decline()}
        </Button>
        <span className="text-muted-foreground">{m.widen_asks_passkey()}</span>
      </span>
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </div>
  );
}
