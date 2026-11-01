import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import type {
  AccessSetting,
  AccountProbe,
  ManagedAccount,
  ManagedSource,
  SourceFolder,
} from "@/api/types";
import { HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Signature, system } from "@/components/voice";
import { fullTime, listTime } from "@/lib/format";
import { useNow } from "@/lib/now";
import { cn } from "@/lib/utils";
import { withElevation } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { problem } from "@/send/outgoing";

import {
  accountBody,
  accountForm,
  bytes,
  folderProgress,
  gmailHost,
  sourceDestination,
  sourceWidens,
  stateLabel,
  validAccountForm,
  type AccountForm,
  type Backfill,
} from "./pull";
import {
  Dates,
  Field,
  inputClass,
  PrincipalInput,
  SectionHeader,
  selectClass,
  validPrincipal,
} from "./section";
import { LockedNote, SecretLine } from "./secret-state";

// External mailboxes (§9.5, §13.8): the mailboxes yuuka pulls over IMAP,
// how far each got, and who may read each account, the catch-all's too.
// Adding one, a new password or server, more folders or older mail,
// resuming, and letting someone read an account ask for the passkey;
// pausing, removing and narrowing do not. The password field is
// write-only. Everything here is a person's: no agent sees it.

const touched = ["/api/v1/admin/accounts", "/api/v1/admin/secrets"];

function useRefresh() {
  const qc = useQueryClient();
  return () => {
    for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
  };
}

export function AccountsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/accounts", {});
  const [adding, setAdding] = useState(false);
  const [removed, setRemoved] = useState(false);
  const all = q.data?.accounts ?? [];
  const pulled = all.filter((a) => a.source);
  const shown = removed ? pulled : pulled.filter((a) => a.source?.state !== "removed");
  const others = all.filter((a) => !a.source);
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_accounts()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={adding}
          disabled={!q.data}
          onClick={() => setAdding((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.pull_add()}
        </Button>
      </SectionHeader>
      <p className="text-small text-muted-foreground">{m.pull_hint()}</p>
      {q.data?.locked && <LockedNote text={m.pull_locked()} />}
      {adding && (
        <AccountEditor
          names={all.map((a) => a.name)}
          locked={!!q.data?.locked}
          onDone={() => setAdding(false)}
        />
      )}
      {pulled.length > shown.length || removed ? (
        <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
          <input
            type="checkbox"
            className="accent-current"
            checked={removed}
            onChange={(e) => setRemoved(e.target.checked)}
          />
          {m.pull_show_removed({
            n: pulled.length - pulled.filter((a) => a.source?.state !== "removed").length,
          })}
        </label>
      ) : null}
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground">{m.pull_none()}</p>
      ) : (
        <ul aria-label={m.settings_accounts()} className="flex flex-col border-t">
          {shown.map((a) => (
            <AccountRow key={a.id} a={a} locked={q.data.locked} />
          ))}
        </ul>
      )}
      {others.length > 0 && (
        <section className="flex flex-col gap-1.5 pt-2">
          <h3 className="text-ui font-medium">{m.pull_other_accounts()}</h3>
          <p className="text-small text-muted-foreground">{m.pull_other_hint()}</p>
          <ul className="flex flex-col border-t">
            {others.map((a) => (
              <li key={a.id} className="flex flex-col gap-1 border-b px-2 py-2">
                <p className="flex flex-wrap items-baseline gap-x-2 text-ui">
                  <span className="font-mono font-medium">{a.name}</span>
                  <span className="text-meta text-muted-foreground">{m.pull_kind_catchall()}</span>
                </p>
                <Readers a={a} />
              </li>
            ))}
          </ul>
        </section>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.pull_on_host()}
          command="yuuka account add NAME --imap --preset gmail --address ADDRESS --password-file -"
        />
      </details>
    </div>
  );
}

function AccountRow({ a, locked }: { a: ManagedAccount; locked: boolean }) {
  const s = a.source as ManagedSource;
  const refresh = useRefresh();
  const [editing, setEditing] = useState(false);
  const [probe, setProbe] = useState<AccountProbe>();
  const [busy, setBusy] = useState<string>();
  const [err, setErr] = useState<unknown>();
  const [asked, setAsked] = useState(false);
  const off = s.state === "removed";
  const st = stateLabel(s.state);
  const name = { params: { path: { name: a.name } } };
  const run = async (what: string, call: () => Promise<unknown>) => {
    setBusy(what);
    setErr(undefined);
    try {
      await call();
      refresh();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(undefined);
    }
  };
  const test = () =>
    run("test", async () => {
      setProbe(undefined);
      setProbe(await data(api.POST("/api/v1/admin/accounts/{name}/test", name)));
    });
  const sync = (retry: boolean) =>
    run("sync", async () => {
      await data(
        api.POST("/api/v1/admin/accounts/{name}/sync", { ...name, body: { retry_failed: retry } }),
      );
      setAsked(true);
    });
  const revision = { ...name, body: { revision: s.revision } };
  return (
    <li className={cn("flex flex-col gap-1.5 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{a.name}</span>
        <span className="min-w-0 truncate font-mono text-small">{s.address}</span>
        <span
          className={cn(
            "text-meta font-medium",
            st.tone === "ok" && "text-system-foreground",
            st.tone === "warn" && "text-warning",
          )}
        >
          {off ? m.pull_removed({ when: listTime(s.disabled_at ?? "") }) : st.text}
        </span>
      </p>
      <StateBanner s={s} name={a.name} />
      <Dates
        items={[
          [m.pull_server(), sourceDestination(s)],
          [m.pull_dates_preset(), s.preset === "gmail" ? m.pull_preset_gmail() : undefined],
          [
            m.pull_dates_folders(),
            s.mailboxes?.length ? s.mailboxes.join(", ") : m.pull_folders_default(),
          ],
          [
            m.pull_dates_backfill(),
            s.backfill_since
              ? m.pull_since({ when: listTime(s.backfill_since) })
              : m.pull_backfill_all(),
          ],
          [m.pull_max_size(), bytes(s.max_message_bytes)],
          [
            m.pull_budget(),
            s.daily_byte_budget
              ? m.pull_per_day({ size: bytes(s.daily_byte_budget) })
              : m.pull_no_limit(),
          ],
          [m.pull_poll(), m.pull_every({ n: String(s.poll_seconds) })],
        ]}
      />
      {!off &&
        (s.trusted_authserv_id ? (
          <p className="text-meta text-muted-foreground">
            {m.pull_trusts({ id: s.trusted_authserv_id })}
          </p>
        ) : (
          <p className="text-meta text-warning">{m.pull_trusts_none()}</p>
        ))}
      {s.secret && <SecretLine label={m.pull_password()} s={s.secret} />}
      <Progress folders={s.folders} />
      {s.failures.length > 0 && (
        <details className="text-small">
          <summary className="cursor-pointer text-warning">
            {m.pull_failures({ n: s.failures.length })}
          </summary>
          <ul className="mt-1 flex flex-col gap-0.5">
            {s.failures.map((f) => (
              <li key={`${f.folder}/${f.uid}`} className="flex flex-wrap gap-x-2 text-meta">
                <span className="font-mono">
                  {m.pull_failure_at({ folder: f.folder, uid: String(f.uid) })}
                </span>
                {f.size >= 0 && <span className="tabular-nums">{bytes(f.size)}</span>}
                <span className="text-muted-foreground">
                  {m.pull_failure_tries({ n: f.attempts, when: listTime(f.last_at) })}
                </span>
                {/* The server's words: data, shown as such. */}
                <span className="min-w-0 font-mono break-all text-muted-foreground">
                  {f.detail}
                </span>
              </li>
            ))}
          </ul>
        </details>
      )}
      {editing ? (
        <AccountEditor a={a} locked={locked} onDone={() => setEditing(false)} />
      ) : (
        !off && (
          <div className="flex flex-wrap items-start gap-2 pt-0.5">
            <Button size="xs" variant="outline" onClick={() => setEditing(true)}>
              {m.pull_edit()}
            </Button>
            <Button size="xs" variant="outline" disabled={!!busy} onClick={() => void test()}>
              {busy === "test" ? m.pull_testing() : m.pull_test()}
            </Button>
            <Button size="xs" variant="outline" disabled={!!busy} onClick={() => void sync(false)}>
              {m.pull_sync()}
            </Button>
            {s.failures.length > 0 && (
              <Button size="xs" variant="outline" disabled={!!busy} onClick={() => void sync(true)}>
                {m.pull_retry_failed()}
              </Button>
            )}
            {s.paused_at ? (
              <Button
                size="xs"
                variant="outline"
                disabled={!!busy}
                onClick={() =>
                  void run("resume", () =>
                    withElevation(() =>
                      data(api.POST("/api/v1/admin/accounts/{name}/resume", revision)),
                    ),
                  )
                }
              >
                {m.pull_resume()}
              </Button>
            ) : (
              <Button
                size="xs"
                variant="outline"
                disabled={!!busy}
                onClick={() =>
                  void run("pause", () =>
                    data(api.POST("/api/v1/admin/accounts/{name}/pause", revision)),
                  )
                }
              >
                {m.pull_pause()}
              </Button>
            )}
            <ConfirmButton
              label={m.pull_remove()}
              confirmLabel={m.pull_remove_confirm()}
              cost={m.pull_remove_cost({ name: a.name, address: s.address })}
              onConfirm={async () => {
                await data(api.POST("/api/v1/admin/accounts/{name}/disable", revision));
                refresh();
              }}
            />
          </div>
        )
      )}
      {asked && (
        <p role="status" className="text-small text-muted-foreground">
          {s.paused_at ? m.pull_sync_asked_paused() : m.pull_sync_asked()}
        </p>
      )}
      {probe && <ProbeResult probe={probe} user={s.username} />}
      {err !== undefined && <ProblemLine p={problem(err)} />}
      <Readers a={a} />
    </li>
  );
}

/** Why a source is not pulled, and what to do, said by the system. */
function StateBanner({ s, name }: { s: ManagedSource; name: string }) {
  if (s.state !== "login_refused" && s.state !== "paused") return null;
  return (
    <p
      role="status"
      className={cn(
        "flex flex-wrap items-baseline gap-x-2 rounded-md border px-2 py-1.5 text-small",
        s.state === "login_refused"
          ? "border-warning/50 bg-warning/10"
          : "border-system-border bg-system-surface",
      )}
    >
      <Signature speaker={system} />
      <span>
        {s.state === "login_refused"
          ? m.pull_banner_refused({ name })
          : m.pull_banner_paused({ when: listTime(s.paused_at ?? "") })}
      </span>
    </p>
  );
}

/** How far each folder got: new mail caught up to, old mail backfilled down to, when last looked at. */
function Progress({ folders }: { folders: SourceFolder[] }) {
  if (folders.length === 0)
    return <p className="text-small text-muted-foreground">{m.pull_not_synced()}</p>;
  return (
    <ul aria-label={m.pull_progress()} className="flex flex-col gap-0.5 text-small">
      {folders.map((f) => {
        const p = folderProgress(f);
        return (
          <li key={f.name} className="flex flex-wrap items-baseline gap-x-3">
            <span className="font-mono">{f.name}</span>
            <span className={cn(p.done && "text-system-foreground")}>{p.catchUp}</span>
            <span>{p.backfill}</span>
            {f.messages > 0 && (
              <span className="text-meta text-muted-foreground tabular-nums">
                {m.pull_on_server({ n: f.messages })}
              </span>
            )}
            {f.drafts_skipped > 0 && (
              <span className="text-meta text-muted-foreground tabular-nums">
                {m.pull_drafts_skipped({ n: f.drafts_skipped })}
              </span>
            )}
            {f.rematching && <span className="text-meta text-warning">{m.pull_rematching()}</span>}
            <span
              className="text-meta text-muted-foreground tabular-nums"
              title={f.last_round_at ? fullTime(f.last_round_at) : undefined}
            >
              {f.last_round_at
                ? m.pull_last_round({ when: listTime(f.last_round_at) })
                : m.pull_no_round()}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

function ProbeResult({ probe, user }: { probe: AccountProbe; user: string }) {
  return (
    <div role="status" className="flex flex-col gap-0.5 text-small">
      <p className={probe.ok ? "text-system-foreground" : "text-warning"}>
        {probe.ok
          ? m.pull_test_ok({ user, n: probe.folders.length })
          : probe.login_refused
            ? m.pull_test_refused()
            : m.pull_test_failed()}{" "}
        {probe.error && <span className="font-mono break-all">{probe.error}</span>}
      </p>
      {probe.folders.map((f) => (
        <p
          key={f.name}
          className="flex flex-wrap gap-x-2 text-meta text-muted-foreground tabular-nums"
        >
          <span className="font-mono">{f.name}</span>
          {f.special_use && <span className="font-mono">{f.special_use}</span>}
          <span>{m.pull_on_server({ n: f.messages })}</span>
        </p>
      ))}
      {probe.missing.map((x) => (
        <p key={x} className="text-warning">
          {m.pull_test_missing({ folder: x })}
        </p>
      ))}
      {probe.problems.map((x) => (
        <p key={x} className="text-warning">
          {x}
        </p>
      ))}
    </div>
  );
}

const backfillLabel: Record<Backfill, () => string> = {
  keep: m.pull_backfill_keep,
  all: m.pull_backfill_all,
  none: m.pull_backfill_none,
  days: m.pull_backfill_days,
};

/**
 * A pull account's whole configuration, as `yuuka account add` takes it:
 * a new one when a is absent, else a's source at the revision listed.
 * The password is never shown; blank keeps the one set.
 */
function AccountEditor({
  a,
  names = [],
  locked,
  onDone,
}: {
  a?: ManagedAccount;
  names?: string[];
  locked: boolean;
  onDone: () => void;
}) {
  const refresh = useRefresh();
  const cur = a?.source;
  const [f, setF] = useState<AccountForm>(() => accountForm(cur, a?.name));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const set = (patch: Partial<AccountForm>) => setF((x) => ({ ...x, ...patch }));
  const isNew = !a;
  const taken = isNew && names.includes(f.name);
  const ready = validAccountForm(f, isNew) && !taken && !(locked && f.password);
  // A minute is fine enough for "the last 30 days" in the passkey note.
  const now = useNow(60_000);
  const widens = sourceWidens(cur, accountBody(f, cur, new Date(now)));
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      const put = () =>
        data(
          api.PUT("/api/v1/admin/accounts/{name}", {
            params: { path: { name: f.name } },
            body: accountBody(f, cur, new Date()),
          }),
        );
      await withElevation(put);
      refresh();
      onDone();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const destination = sourceDestination({
    username: f.username.trim() || f.address.trim().toLowerCase(),
    host: f.host.trim().toLowerCase() || (f.preset === "gmail" ? gmailHost : ""),
    port: Number(f.port) || 993,
  });
  return (
    <form
      className="flex flex-col gap-2 rounded-md border px-3 py-2.5 text-small"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <p className="flex flex-wrap items-baseline gap-x-2">
        <span className="font-medium">
          {isNew ? m.pull_add_legend() : m.pull_edit_legend({ name: a.name })}
        </span>
        <span className="text-muted-foreground">
          {widens ? m.widen_asks_passkey() : m.pull_tightens()}
        </span>
      </p>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        {isNew && (
          <Field label={m.pull_name()} className="w-36">
            <input
              className={inputClass}
              value={f.name}
              placeholder="gmail"
              onChange={(e) => set({ name: e.target.value.trim() })}
            />
          </Field>
        )}
        <Field label={m.pull_preset()} className="w-36">
          <select
            className={selectClass}
            value={f.preset}
            onChange={(e) => set({ preset: e.target.value as AccountForm["preset"] })}
          >
            <option value="gmail">{m.pull_preset_gmail()}</option>
            <option value="">{m.pull_preset_none()}</option>
          </select>
        </Field>
        <Field label={m.pull_address()} className="w-60">
          <input
            className={inputClass}
            value={f.address}
            placeholder="me@example.com"
            onChange={(e) => set({ address: e.target.value.trim() })}
          />
        </Field>
      </div>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <Field label={m.pull_host()} className="w-52">
          <input
            className={inputClass}
            value={f.host}
            placeholder={f.preset === "gmail" ? gmailHost : "imap.example.com"}
            onChange={(e) => set({ host: e.target.value })}
          />
        </Field>
        <Field label={m.pull_port()} className="w-20">
          <input
            className={inputClass}
            inputMode="numeric"
            value={f.port}
            onChange={(e) => set({ port: e.target.value.trim() })}
          />
        </Field>
        <Field label={m.pull_username()} className="w-60">
          <input
            className={inputClass}
            value={f.username}
            autoComplete="off"
            placeholder={f.address.toLowerCase() || m.pull_username_default()}
            onChange={(e) => set({ username: e.target.value.trim() })}
          />
        </Field>
        <Field
          label={m.pull_password()}
          className="w-52"
          hint={
            cur?.secret
              ? m.pull_password_keep()
              : f.preset === "gmail"
                ? m.pull_app_password()
                : undefined
          }
        >
          <input
            type="password"
            className={inputClass}
            value={f.password}
            autoComplete="new-password"
            disabled={locked}
            placeholder={cur?.secret ? "••••••••" : ""}
            onChange={(e) => set({ password: e.target.value })}
          />
        </Field>
      </div>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <Field label={m.pull_folders()} className="w-60" hint={m.pull_folders_hint()}>
          <input
            className={inputClass}
            value={f.mailboxes}
            placeholder={f.preset === "gmail" ? "\\All" : m.pull_folders_default()}
            onChange={(e) => set({ mailboxes: e.target.value })}
          />
        </Field>
        <Field label={m.pull_backfill()} className="w-44">
          <span className="flex items-center gap-1">
            <select
              className={selectClass}
              value={f.backfill}
              onChange={(e) => set({ backfill: e.target.value as Backfill })}
            >
              {(isNew
                ? (["all", "none", "days"] as const)
                : (["keep", "all", "none", "days"] as const)
              ).map((b) => (
                <option key={b} value={b}>
                  {backfillLabel[b]()}
                </option>
              ))}
            </select>
            {f.backfill === "days" && (
              <input
                aria-label={m.pull_backfill_days()}
                className={cn(inputClass, "w-16")}
                inputMode="numeric"
                value={f.days}
                onChange={(e) => set({ days: e.target.value.trim() })}
              />
            )}
          </span>
        </Field>
      </div>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <Field label={m.pull_max_size_mib()} className="w-32" hint={m.pull_max_size_hint()}>
          <input
            className={inputClass}
            inputMode="decimal"
            value={f.maxMiB}
            placeholder="32"
            onChange={(e) => set({ maxMiB: e.target.value.trim() })}
          />
        </Field>
        <Field label={m.pull_budget_mib()} className="w-32" hint={m.pull_budget_hint()}>
          <input
            className={inputClass}
            inputMode="decimal"
            value={f.budgetMiB}
            placeholder={f.preset === "gmail" ? "2048" : "0"}
            onChange={(e) => set({ budgetMiB: e.target.value.trim() })}
          />
        </Field>
        <Field label={m.pull_poll_seconds()} className="w-28">
          <input
            className={inputClass}
            inputMode="numeric"
            value={f.poll}
            onChange={(e) => set({ poll: e.target.value.trim() })}
          />
        </Field>
        <Field label={m.pull_authserv()} className="w-52" hint={m.pull_authserv_hint()}>
          <input
            className={inputClass}
            value={f.authservId}
            placeholder={f.preset === "gmail" ? "mx.google.com" : ""}
            onChange={(e) => set({ authservId: e.target.value.trim() })}
          />
        </Field>
      </div>
      {taken && <p className="text-warning">{m.pull_name_taken({ name: f.name })}</p>}
      {locked && <p className="text-warning">{m.pull_locked_password()}</p>}
      {f.address.includes("@") && (
        <p className="text-muted-foreground">{m.pull_cost({ destination, address: f.address })}</p>
      )}
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={!ready || busy}>
          {isNew ? m.pull_add_submit() : m.pull_save()}
        </Button>
        <Button type="button" size="xs" variant="ghost" onClick={onDone}>
          {m.rule_cancel()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </form>
  );
}

/**
 * Who may read an account (§13.8): its default for principals without a
 * setting of their own, and their settings. Letting someone read asks
 * for the passkey; taking it away does not.
 */
function Readers({ a }: { a: ManagedAccount }) {
  const refresh = useRefresh();
  const [adding, setAdding] = useState(false);
  const [who, setWho] = useState("agent:");
  const [setting, setSetting] = useState<AccessSetting>("read");
  const [err, setErr] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const run = async (call: () => Promise<unknown>) => {
    setBusy(true);
    setErr(undefined);
    try {
      await withElevation(call);
      refresh();
      return true;
    } catch (e) {
      setErr(e);
      return false;
    } finally {
      setBusy(false);
    }
  };
  const access = (principal: string, to: AccessSetting) =>
    run(() =>
      data(
        api.PUT("/api/v1/admin/accounts/{name}/access/{principal}", {
          params: { path: { name: a.name, principal } },
          body: { access: to },
        }),
      ),
    );
  const setDefault = (to: boolean) =>
    run(() =>
      data(
        api.PUT("/api/v1/admin/accounts/{name}/default-read", {
          params: { path: { name: a.name } },
          body: { default_read: to },
        }),
      ),
    );
  return (
    <section aria-label={m.pull_readers()} className="flex flex-col gap-1 text-small">
      <p className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="font-medium">{m.pull_readers()}</span>
        <span className="text-muted-foreground">
          {a.default_read ? m.pull_default_readable() : m.pull_default_closed()}
        </span>
        <Button
          size="xs"
          variant="ghost"
          disabled={busy}
          onClick={() => void setDefault(!a.default_read)}
        >
          {a.default_read ? m.pull_default_close() : m.pull_default_open()}
        </Button>
        <Button
          size="xs"
          variant="ghost"
          aria-expanded={adding}
          onClick={() => setAdding((v) => !v)}
        >
          {m.pull_add_reader()}
        </Button>
      </p>
      {a.access.length > 0 && (
        <ul className="flex flex-col gap-0.5">
          {a.access.map((x) => (
            <li key={x.principal} className="flex flex-wrap items-center gap-x-2">
              <span className="font-mono">{x.principal}</span>
              <span className={x.can_read ? "text-system-foreground" : "text-muted-foreground"}>
                {x.can_read ? m.pull_may_read() : m.pull_may_not_read()}
              </span>
              <Button
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => void access(x.principal, x.can_read ? "no_read" : "read")}
              >
                {x.can_read ? m.pull_take_away() : m.pull_let_read()}
              </Button>
              <Button
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => void access(x.principal, "default")}
              >
                {m.pull_to_default()}
              </Button>
            </li>
          ))}
        </ul>
      )}
      {adding && (
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void access(who, setting).then((ok) => ok && setAdding(false));
          }}
        >
          <PrincipalInput label={m.pull_principal()} value={who} onChange={setWho} />
          <select
            aria-label={m.pull_setting()}
            className={selectClass}
            value={setting}
            onChange={(e) => setSetting(e.target.value as AccessSetting)}
          >
            <option value="read">{m.pull_may_read()}</option>
            <option value="no_read">{m.pull_may_not_read()}</option>
          </select>
          <Button type="submit" size="xs" variant="outline" disabled={busy || !validPrincipal(who)}>
            {m.pull_set_access()}
          </Button>
        </form>
      )}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </section>
  );
}
