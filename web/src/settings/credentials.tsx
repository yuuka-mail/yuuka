import { useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { useState } from "react";

import { $api, api, data } from "@/api/client";
import type { IssuedCredential, ManagedCredential, Scope } from "@/api/types";
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

import { allScopes } from "./authority";
import {
  Dates,
  Field,
  inputClass,
  PrincipalInput,
  SecretOnce,
  SectionHeader,
  selectClass,
  validPrincipal,
  WideningForm,
} from "./section";

// Credentials (§13.3, §13.10): the tokens and SMTP passwords of agents
// and services, never their secrets. Sessions are not listed; signing
// out ends one. Revoking takes one step; issuing asks for the passkey
// first, and shows the secret this once.

const credentialsKey = ["get", "/api/v1/admin/credentials"];

export function CredentialsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/credentials", {});
  const [revoked, setRevoked] = useState(false);
  const [issuing, setIssuing] = useState(false);
  const [issued, setIssued] = useState<IssuedCredential>();
  const all = q.data?.credentials ?? [];
  const shown = revoked ? all : all.filter((c) => !c.revoked_at);
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_credentials()}>
        <Button
          size="sm"
          variant="outline"
          aria-expanded={issuing}
          onClick={() => setIssuing((v) => !v)}
        >
          <PlusIcon data-icon="inline-start" />
          {m.credentials_issue()}
        </Button>
      </SectionHeader>
      {issued && (
        <SecretOnce
          lines={
            issued.smtp_username
              ? [
                  { label: m.credentials_smtp_username(), value: issued.smtp_username },
                  { label: m.credentials_smtp_password(), value: issued.secret },
                ]
              : [{ label: m.credentials_token(), value: issued.secret }]
          }
          onDone={() => setIssued(undefined)}
        />
      )}
      {issuing && (
        <IssueForm
          onIssued={(c) => {
            setIssuing(false);
            setIssued(c);
          }}
        />
      )}
      <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
        <input
          type="checkbox"
          className="accent-current"
          checked={revoked}
          onChange={(e) => setRevoked(e.target.checked)}
        />
        {m.credentials_show_revoked({ n: all.length - all.filter((c) => !c.revoked_at).length })}
      </label>
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground">{m.credentials_none()}</p>
      ) : (
        <ul aria-label={m.settings_credentials()} className="flex flex-col border-t">
          {shown.map((c) => (
            <CredentialRow key={c.id} c={c} />
          ))}
        </ul>
      )}
      <details className="text-small">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          {m.settings_on_host()}
        </summary>
        <HostCommand
          className="mt-1"
          why={m.credentials_on_host()}
          command="yuuka token issue --principal KIND:NAME --scope SCOPE,SCOPE"
        />
      </details>
    </div>
  );
}

const lifetimes: { days: number; label: () => string }[] = [
  { days: 0, label: m.credentials_ttl_never },
  { days: 30, label: () => m.credentials_ttl_days({ n: 30 }) },
  { days: 90, label: () => m.credentials_ttl_days({ n: 90 }) },
  { days: 365, label: () => m.credentials_ttl_days({ n: 365 }) },
];

/** Issue a token or an SMTP password: every scope named, no defaults (§13.3). */
function IssueForm({ onIssued }: { onIssued: (c: IssuedCredential) => void }) {
  const qc = useQueryClient();
  const [kind, setKind] = useState<"token" | "smtp_password">("token");
  const [who, setWho] = useState("agent:");
  const [label, setLabel] = useState("");
  const [scopes, setScopes] = useState<Scope[]>([]);
  const [days, setDays] = useState(0);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>();
  const smtp = kind === "smtp_password";
  const ready = validPrincipal(who) && !who.startsWith("human:") && (smtp || scopes.length > 0);
  const submit = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      const c = await withElevation(() =>
        data(
          api.POST("/api/v1/admin/credentials", {
            body: {
              kind,
              principal: who,
              label: label.trim() || undefined,
              scopes: smtp ? undefined : allScopes.filter((s) => scopes.includes(s)),
              ttl_seconds: days ? days * 86400 : undefined,
            },
          }),
        ),
      );
      void qc.invalidateQueries({ queryKey: credentialsKey });
      onIssued(c);
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <WideningForm legend={m.credentials_issue_legend()} onSubmit={() => void submit()}>
      <div className="flex flex-wrap items-end gap-x-4 gap-y-2">
        <fieldset className="flex flex-col gap-0.5">
          <legend className="text-muted-foreground">{m.credentials_kind()}</legend>
          <span className="flex h-7 items-center gap-3">
            {(["token", "smtp_password"] as const).map((k) => (
              <label key={k} className="flex items-center gap-1">
                <input
                  type="radio"
                  className="accent-current"
                  checked={kind === k}
                  onChange={() => setKind(k)}
                />
                {k === "token" ? m.credentials_kind_token() : m.credentials_kind_smtp()}
              </label>
            ))}
          </span>
        </fieldset>
        <PrincipalInput
          label={m.credentials_for()}
          kinds={["agent", "service"]}
          value={who}
          onChange={setWho}
        />
        <Field label={m.credentials_label()} className="w-56">
          <input
            className={cn(inputClass, "font-sans")}
            value={label}
            placeholder={m.credentials_label_hint()}
            onChange={(e) => setLabel(e.target.value)}
          />
        </Field>
        <Field label={m.credentials_lifetime()}>
          <select
            className={selectClass}
            value={days}
            onChange={(e) => setDays(Number(e.target.value))}
          >
            {lifetimes.map((l) => (
              <option key={l.days} value={l.days}>
                {l.label()}
              </option>
            ))}
          </select>
        </Field>
      </div>
      {smtp ? (
        <p className="text-muted-foreground">{m.credentials_smtp_scope()}</p>
      ) : (
        <fieldset className="flex flex-col gap-1">
          <legend className="text-muted-foreground">{m.credentials_scopes()}</legend>
          <span className="flex flex-wrap gap-x-3 gap-y-1">
            {allScopes.map((s) => (
              <label key={s} className="inline-flex items-center gap-1 font-mono">
                <input
                  type="checkbox"
                  className="accent-current"
                  checked={scopes.includes(s)}
                  onChange={(e) =>
                    setScopes(e.target.checked ? [...scopes, s] : scopes.filter((x) => x !== s))
                  }
                />
                {s}
              </label>
            ))}
          </span>
          <span className="text-meta text-muted-foreground">{m.credentials_scopes_hint()}</span>
        </fieldset>
      )}
      <span className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="xs" disabled={!ready || busy}>
          {m.credentials_issue_submit()}
        </Button>
        {err !== undefined && <ProblemLine p={problem(err)} />}
      </span>
    </WideningForm>
  );
}

function CredentialRow({ c }: { c: ManagedCredential }) {
  const qc = useQueryClient();
  const off = !!c.revoked_at;
  const who = principal(c.principal);
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{who}</span>
        <span className="text-meta text-muted-foreground uppercase">
          {c.kind === "smtp_password" ? m.credentials_kind_smtp() : m.credentials_kind_token()}
        </span>
        {c.label && <span className="min-w-0 truncate">{c.label}</span>}
        {c.principal_disabled && (
          <span className="text-meta text-warning">{m.credentials_principal_disabled()}</span>
        )}
        {off && (
          <span className="text-meta">
            {m.credentials_revoked({ when: listTime(c.revoked_at ?? "") })}
          </span>
        )}
        <span className="ml-auto font-mono text-meta text-muted-foreground">{c.id}</span>
      </p>
      <p className="font-mono text-small wrap-anywhere">{c.scopes.join(" ")}</p>
      <Dates
        items={[
          [m.credentials_created(), listTime(c.created_at)],
          [m.credentials_expires(), c.expires_at && listTime(c.expires_at)],
          [
            m.credentials_last_used(),
            c.last_used_at ? listTime(c.last_used_at) : m.credentials_never_used(),
          ],
        ]}
      />
      {!off && (
        <div className="pt-0.5">
          <ConfirmButton
            label={m.credentials_revoke()}
            confirmLabel={m.credentials_revoke_confirm()}
            cost={
              c.kind === "smtp_password"
                ? m.credentials_revoke_cost_smtp({ who })
                : m.credentials_revoke_cost({ who })
            }
            onConfirm={async () => {
              await api.POST("/api/v1/admin/credentials/{id}/revoke", {
                params: { path: { id: c.id } },
              });
              void qc.invalidateQueries({ queryKey: credentialsKey });
            }}
          />
        </div>
      )}
    </li>
  );
}
