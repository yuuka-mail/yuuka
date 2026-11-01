import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import { useSession } from "@/api/queries";
import type { IdentityAdminList, ManagedIdentity } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { withElevation } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { principal, problem } from "@/send/outgoing";

import { chainWidens } from "./thirdparty";

import {
  Dates,
  Field,
  inputClass,
  PrincipalInput,
  SectionHeader,
  selectClass,
  validPrincipal,
  WideningForm,
} from "./section";

// Identities (§9.3, §13.10): the addresses yuuka sends as. Disabling one
// is a tightening, except where the zone's *@zone pattern would take the
// address over with its own owner and grants; that, and adding one, ask
// for the passkey first. So does a transport chain (§10.2.2, §17.6) that
// lets mail go anywhere new; taking a fallback away does not.

const touched = ["/api/v1/admin/identities", "/api/v1/admin/grants", "/api/v1/identities"];

export function IdentitiesSection() {
  const q = $api.useQuery("get", "/api/v1/admin/identities", {});
  const [adding, setAdding] = useState(false);
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_identities()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={adding}
          disabled={!q.data}
          onClick={() => setAdding((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.identities_add()}
        </Button>
      </SectionHeader>
      {adding && q.data && <AddForm list={q.data} onAdded={() => setAdding(false)} />}
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : q.data.identities.length === 0 ? (
        <p className="text-muted-foreground">{m.identities_none()}</p>
      ) : (
        <ul aria-label={m.settings_identities()} className="flex flex-col border-t">
          {q.data.identities.map((i) => (
            <IdentityRow key={i.id} i={i} transports={q.data.transports} />
          ))}
        </ul>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.identities_on_host()}
          command="yuuka identity add LOCAL@ZONE --owner KIND:NAME --transport NAME"
        />
      </details>
    </div>
  );
}

/** Add an identity: an address, who owns it, the exit it goes out through. */
function AddForm({ list, onAdded }: { list: IdentityAdminList; onAdded: () => void }) {
  const qc = useQueryClient();
  const me = useSession().data?.principal ?? "human:";
  const [address, setAddress] = useState("");
  const [owner, setOwner] = useState(me);
  const [transport, setTransport] = useState(list.transports[0] ?? "");
  const [account, setAccount] = useState(list.accounts.length === 1 ? list.accounts[0] : "");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  if (list.transports.length === 0)
    return (
      <p className="rounded-md border px-3 py-2 text-small text-muted-foreground">
        {m.identities_no_transport()}
      </p>
    );
  const ready =
    /^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(address.trim()) &&
    validPrincipal(owner) &&
    !!transport &&
    !!account;
  const pattern = address.trim().startsWith("*@");
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await withElevation(() =>
        data(
          api.POST("/api/v1/admin/identities", {
            body: {
              address: address.trim(),
              owner,
              transport,
              account,
              display_name: name.trim() || undefined,
            },
          }),
        ),
      );
      for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
      onAdded();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <WideningForm legend={m.identities_add_legend()} onSubmit={() => void submit()}>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <Field label={m.identities_address()} className="w-64">
          <input
            className={inputClass}
            value={address}
            title={m.identities_address_hint()}
            placeholder="ops@example.com, *@example.com"
            onChange={(e) => setAddress(e.target.value)}
          />
        </Field>
        <PrincipalInput label={m.identities_field_owner()} value={owner} onChange={setOwner} />
        <Field label={m.identities_field_transport()}>
          <select
            className={selectClass}
            value={transport}
            onChange={(e) => setTransport(e.target.value)}
          >
            {list.transports.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </Field>
        {list.accounts.length > 1 && (
          <Field label={m.identities_account()}>
            <select
              className={selectClass}
              value={account}
              onChange={(e) => setAccount(e.target.value)}
            >
              <option value="" disabled>
                —
              </option>
              {list.accounts.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </Field>
        )}
        <Field label={m.identities_display_name()} className="w-48">
          <input
            className={cn(inputClass, "font-sans")}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
      </div>
      <p className="text-muted-foreground">
        {pattern
          ? m.identities_pattern_cost({ zone: address.trim().slice(2) })
          : owner.startsWith("human:")
            ? m.identities_human_owner({ owner })
            : m.identities_own_owner({ owner })}
      </p>
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={!ready || busy}>
          {m.identities_add_submit()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </WideningForm>
  );
}

function IdentityRow({ i, transports }: { i: ManagedIdentity; transports: string[] }) {
  const qc = useQueryClient();
  const [chain, setChain] = useState(false);
  const off = !!i.disabled_at;
  const disable = async () => {
    await withElevation(() =>
      data(api.POST("/api/v1/admin/identities/{id}/disable", { params: { path: { id: i.id } } })),
    );
    for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
  };
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{i.address}</span>
        {i.display_name && <span className="min-w-0 truncate">{i.display_name}</span>}
        {off && (
          <span className="text-meta">
            {m.identities_disabled({ when: listTime(i.disabled_at ?? "") })}
          </span>
        )}
      </p>
      <Dates
        items={[
          [m.identities_owner(), principal(i.owner)],
          [m.identities_transport(), i.transports.join(" → ")],
          [m.credentials_created(), listTime(i.created_at)],
        ]}
      />
      {!off && chain && <ChainEditor i={i} options={transports} onDone={() => setChain(false)} />}
      {!off && !chain && (
        <div className="flex flex-wrap items-start gap-2 pt-0.5">
          <Button size="xs" variant="outline" onClick={() => setChain(true)}>
            {m.chain_edit()}
          </Button>
          <ConfirmButton
            label={m.identities_disable()}
            confirmLabel={m.identities_disable_confirm()}
            cost={
              i.taken_over_by
                ? m.identities_taken_over({ pattern: i.taken_over_by })
                : m.identities_disable_cost({ address: i.address })
            }
            onConfirm={disable}
          />
        </div>
      )}
    </li>
  );
}

/**
 * An identity's transport chain, primary first (§10.2.2): a fallback
 * added, or another primary, lets its mail go somewhere new and asks for
 * the passkey; taking one away, or reordering the fallbacks, does not.
 */
function ChainEditor({
  i,
  options,
  onDone,
}: {
  i: ManagedIdentity;
  options: string[];
  onDone: () => void;
}) {
  const qc = useQueryClient();
  const [chain, setChain] = useState<string[]>(i.transports);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const widens = chainWidens(i.transports, chain);
  const same = chain.join() === i.transports.join();
  const spare = options.filter((t) => !chain.includes(t));
  const move = (k: number, by: number) => {
    const next = [...chain];
    [next[k], next[k + by]] = [next[k + by] ?? "", next[k] ?? ""];
    setChain(next);
  };
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await withElevation(() =>
        data(
          api.PUT("/api/v1/admin/identities/{id}/transports", {
            params: { path: { id: i.id } },
            body: { transports: chain },
          }),
        ),
      );
      for (const k of touched) void qc.invalidateQueries({ queryKey: ["get", k] });
      void qc.invalidateQueries({ queryKey: ["get", "/api/v1/admin/transports"] });
      onDone();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  const body = (
    <>
      <ol className="flex flex-col gap-1">
        {chain.map((t, k) => (
          <li
            key={t}
            className="grid grid-cols-[4rem_minmax(0,1fr)_auto] items-center gap-2 sm:grid-cols-[4rem_minmax(0,12rem)_auto]"
          >
            <span className="text-meta text-muted-foreground">
              {k === 0 ? m.chain_primary() : m.chain_fallback({ n: k })}
            </span>
            <span className="truncate font-mono" title={t}>
              {t}
            </span>
            <span className="flex items-center gap-1">
              <Button
                type="button"
                size="xs"
                variant="ghost"
                aria-label={m.chain_up({ name: t })}
                disabled={k === 0}
                onClick={() => move(k, -1)}
              >
                ↑
              </Button>
              <Button
                type="button"
                size="xs"
                variant="ghost"
                aria-label={m.chain_down({ name: t })}
                disabled={k === chain.length - 1}
                onClick={() => move(k, 1)}
              >
                ↓
              </Button>
              <Button
                type="button"
                size="xs"
                variant="ghost"
                disabled={chain.length === 1}
                onClick={() => setChain(chain.filter((x) => x !== t))}
              >
                {m.chain_remove()}
              </Button>
            </span>
          </li>
        ))}
      </ol>
      {spare.length > 0 && chain.length < 8 && (
        <select
          aria-label={m.chain_add()}
          className={cn(selectClass, "self-start")}
          value=""
          onChange={(e) => e.target.value && setChain([...chain, e.target.value])}
        >
          <option value="">{m.chain_add()}</option>
          {spare.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
      )}
      <p className="text-muted-foreground">
        {same ? m.chain_same() : widens ? m.chain_widens() : m.chain_narrows()}
      </p>
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={same || busy}>
          {m.chain_save()}
        </Button>
        <Button type="button" size="xs" variant="ghost" onClick={onDone}>
          {m.rule_cancel()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </>
  );
  return widens ? (
    <WideningForm legend={m.chain_legend({ address: i.address })} onSubmit={() => void submit()}>
      {body}
    </WideningForm>
  ) : (
    <form
      className="flex flex-col gap-2 rounded-md border px-3 py-2.5 text-small"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <p className="font-medium">{m.chain_legend({ address: i.address })}</p>
      {body}
    </form>
  );
}
