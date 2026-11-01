import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import type { ManagedTransport, TransportCap, TransportProbe } from "@/api/types";
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

import { Dates, Field, inputClass, SectionHeader, selectClass, WideningForm } from "./section";
import { LockedNote, SecretLine } from "./secret-state";
import {
  allCaps,
  capsPresets,
  destination,
  needsPassword,
  presetOf,
  transportForm,
  validTransportForm,
  type CapsPreset,
  type TransportForm,
} from "./thirdparty";

// Transports (§10.2, §17): where mail goes out, and the password each
// one authenticates with, sealed. Configuring one writes a secret or sets
// where mail goes, so it asks for the passkey first; the password field
// is write-only and blank keeps the one set. Removing one is a tightening
// and takes a second press.

const touched = ["/api/v1/admin/transports", "/api/v1/admin/identities", "/api/v1/admin/secrets"];

export function TransportsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/transports", {});
  const [adding, setAdding] = useState(false);
  const [removed, setRemoved] = useState(false);
  const all = q.data?.transports ?? [];
  const shown = removed ? all : all.filter((t) => t.state !== "removed");
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_transports()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={adding}
          disabled={!q.data}
          onClick={() => setAdding((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.transports_add()}
        </Button>
      </SectionHeader>
      <p className="text-small text-muted-foreground">{m.transports_hint()}</p>
      {q.data?.locked && <LockedNote text={m.transports_locked()} />}
      {adding && (
        <TransportEditor
          names={all.map((t) => t.name)}
          onDone={() => setAdding(false)}
          locked={!!q.data?.locked}
        />
      )}
      <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
        <input
          type="checkbox"
          className="accent-current"
          checked={removed}
          onChange={(e) => setRemoved(e.target.checked)}
        />
        {m.transports_show_removed({
          n: all.length - all.filter((t) => t.state !== "removed").length,
        })}
      </label>
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground">{m.transports_none()}</p>
      ) : (
        <ul aria-label={m.settings_transports()} className="flex flex-col border-t">
          {shown.map((t) => (
            <TransportRow key={t.id} t={t} locked={q.data.locked} />
          ))}
        </ul>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.transports_on_host()}
          command="yuuka transport add NAME --host HOST --username USER --password-file - --caps cloudflare"
        />
      </details>
    </div>
  );
}

function TransportRow({ t, locked }: { t: ManagedTransport; locked: boolean }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [probe, setProbe] = useState<TransportProbe>();
  const [probing, setProbing] = useState(false);
  const [err, setErr] = useState<unknown>();
  const off = t.state === "removed";
  const test = async () => {
    setProbing(true);
    setProbe(undefined);
    setErr(undefined);
    try {
      setProbe(
        await data(
          api.POST("/api/v1/admin/transports/{name}/test", { params: { path: { name: t.name } } }),
        ),
      );
    } catch (e) {
      setErr(e);
    } finally {
      setProbing(false);
    }
  };
  const remove = async () => {
    await data(
      api.POST("/api/v1/admin/transports/{name}/disable", {
        params: { path: { name: t.name } },
        body: { revision: t.revision },
      }),
    );
    for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
  };
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{t.name}</span>
        {t.state === "configured" && (
          <span className="min-w-0 truncate font-mono text-small">{destination(t)}</span>
        )}
        {t.state === "placeholder" && (
          <span className="text-meta text-warning">{m.transports_placeholder()}</span>
        )}
        {off && (
          <span className="text-meta">
            {m.transports_removed({ when: listTime(t.disabled_at ?? "") })}
          </span>
        )}
      </p>
      {t.state === "placeholder" && (
        <p className="text-small text-muted-foreground">{m.transports_placeholder_hint()}</p>
      )}
      <Dates
        items={[
          [
            m.transports_caps(),
            t.state === "placeholder"
              ? undefined
              : t.caps.length
                ? t.caps.join(", ")
                : m.caps_none(),
          ],
          [m.transports_used_by(), t.used_by.length ? t.used_by.join(", ") : undefined],
          [m.transports_configured(), t.configured_at ? listTime(t.configured_at) : undefined],
        ]}
      />
      {t.secret ? (
        <SecretLine label={m.transports_password()} s={t.secret} />
      ) : (
        t.state === "configured" &&
        !t.username && <p className="text-small text-muted-foreground">{m.transports_no_auth()}</p>
      )}
      {editing ? (
        <TransportEditor t={t} locked={locked} onDone={() => setEditing(false)} />
      ) : (
        <div className="flex flex-wrap items-start gap-2 pt-0.5">
          <Button size="xs" variant="outline" onClick={() => setEditing(true)}>
            {t.state === "configured"
              ? m.transports_edit()
              : off
                ? m.transports_configure_again()
                : m.transports_configure()}
          </Button>
          {t.state === "configured" && (
            <Button size="xs" variant="outline" disabled={probing} onClick={() => void test()}>
              {probing ? m.transports_testing() : m.transports_test()}
            </Button>
          )}
          {!off && (
            <ConfirmButton
              label={m.transports_remove()}
              confirmLabel={m.transports_remove_confirm()}
              cost={
                t.used_by.length
                  ? m.transports_remove_cost_used({ name: t.name, used: t.used_by.join(", ") })
                  : m.transports_remove_cost({ name: t.name })
              }
              onConfirm={remove}
            />
          )}
        </div>
      )}
      {probe && (
        <p
          role="status"
          className={cn("text-small", probe.ok ? "text-system-foreground" : "text-warning")}
        >
          {probe.ok
            ? t.username
              ? m.transports_test_ok_auth({ user: t.username })
              : m.transports_test_ok()
            : m.transports_test_failed()}{" "}
          {probe.error && <span className="font-mono">{probe.error}</span>}
        </p>
      )}
      {err !== undefined && <ProblemLine p={problem(err)} />}
    </li>
  );
}

const presetLabel: Record<CapsPreset, () => string> = {
  cloudflare: m.caps_cloudflare,
  standard: m.caps_standard,
  custom: m.caps_custom,
};

/**
 * A transport's whole configuration, as `yuuka transport add` takes it:
 * a new one when t is absent, else t at the revision listed. The
 * password is never shown; blank keeps the one set.
 */
function TransportEditor({
  t,
  names = [],
  locked,
  onDone,
}: {
  t?: ManagedTransport;
  names?: string[];
  locked: boolean;
  onDone: () => void;
}) {
  const qc = useQueryClient();
  const [f, setF] = useState<TransportForm>(() => transportForm(t));
  const [preset, setPreset] = useState<CapsPreset>(() => (t ? presetOf(f.caps) : "cloudflare"));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const set = (patch: Partial<TransportForm>) => setF((x) => ({ ...x, ...patch }));
  const isNew = !t;
  const taken = isNew && names.includes(f.name);
  const missing = needsPassword(t, f);
  const ready = validTransportForm(f) && !taken && !missing && !(locked && f.password);
  const caps = preset === "custom" ? f.caps : capsPresets[preset];
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await withElevation(() =>
        data(
          api.PUT("/api/v1/admin/transports/{name}", {
            params: { path: { name: f.name } },
            body: {
              revision: t?.revision ?? 0,
              host: f.host.trim(),
              port: Number(f.port),
              username: f.username.trim() || undefined,
              password: f.password || undefined,
              caps,
            },
          }),
        ),
      );
      for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
      onDone();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const toggleCap = (c: TransportCap, on: boolean) =>
    set({
      caps: on
        ? allCaps.filter((x) => x === c || f.caps.includes(x))
        : f.caps.filter((x) => x !== c),
    });
  return (
    <WideningForm
      legend={isNew ? m.transports_add_legend() : m.transports_edit_legend({ name: t.name })}
      onSubmit={() => void submit()}
    >
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        {isNew && (
          <Field label={m.transports_name()} className="w-40">
            <input
              className={inputClass}
              value={f.name}
              placeholder="relay"
              onChange={(e) => set({ name: e.target.value.trim() })}
            />
          </Field>
        )}
        <Field label={m.transports_host()} className="w-56">
          <input
            className={inputClass}
            value={f.host}
            placeholder="smtp.example.com"
            onChange={(e) => set({ host: e.target.value })}
          />
        </Field>
        <Field label={m.transports_port()} className="w-20">
          <input
            className={inputClass}
            inputMode="numeric"
            value={f.port}
            onChange={(e) => set({ port: e.target.value.trim() })}
          />
        </Field>
        <Field label={m.transports_username()} className="w-48">
          <input
            className={inputClass}
            value={f.username}
            autoComplete="off"
            onChange={(e) => set({ username: e.target.value.trim() })}
          />
        </Field>
        <Field
          label={m.transports_password()}
          className="w-48"
          hint={t?.secret ? m.transports_password_keep() : undefined}
        >
          <input
            type="password"
            className={inputClass}
            value={f.password}
            autoComplete="new-password"
            disabled={!f.username || locked}
            placeholder={t?.secret ? "••••••••" : ""}
            onChange={(e) => set({ password: e.target.value })}
          />
        </Field>
      </div>
      <fieldset className="flex flex-col gap-1">
        <legend className="text-muted-foreground">{m.transports_caps_legend()}</legend>
        <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
          <select
            aria-label={m.transports_caps_legend()}
            className={selectClass}
            value={preset}
            onChange={(e) => setPreset(e.target.value as CapsPreset)}
          >
            {(["cloudflare", "standard", "custom"] as const).map((p) => (
              <option key={p} value={p}>
                {presetLabel[p]()}
              </option>
            ))}
          </select>
          {preset === "custom" ? (
            allCaps.map((c) => (
              <label key={c} className="flex items-center gap-1 font-mono">
                <input
                  type="checkbox"
                  className="accent-current"
                  checked={f.caps.includes(c)}
                  onChange={(e) => toggleCap(c, e.target.checked)}
                />
                {c}
              </label>
            ))
          ) : (
            <span className="font-mono text-muted-foreground">
              {caps.length ? caps.join(", ") : m.caps_none()}
            </span>
          )}
        </span>
        <span className="text-meta text-muted-foreground">{m.transports_caps_hint()}</span>
      </fieldset>
      {taken && <p className="text-warning">{m.transports_name_taken({ name: f.name })}</p>}
      {missing && (
        <p className="text-muted-foreground">
          {t?.secret ? m.transports_password_again() : m.transports_password_needed()}
        </p>
      )}
      {locked && f.username && <p className="text-warning">{m.transports_locked_password()}</p>}
      <p className="text-muted-foreground">
        {m.transports_cost({ destination: destination({ ...f, port: Number(f.port) }) })}
      </p>
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={!ready || busy}>
          {isNew ? m.transports_add_submit() : m.transports_save()}
        </Button>
        <Button type="button" size="xs" variant="ghost" onClick={onDone}>
          {m.rule_cancel()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </WideningForm>
  );
}
