import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { $api, api } from "@/api/client";
import type { ManagedCredential } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { principal, problem } from "@/send/outgoing";

import { Dates, SectionHeader } from "./section";

// Credentials (§13.3, §13.10): the tokens and SMTP passwords of agents
// and services, never their secrets. Sessions are not listed; signing
// out ends one. Revoking is here; issuing is on the host.

export function CredentialsSection() {
  const q = $api.useQuery("get", "/api/v1/admin/credentials", {});
  const [revoked, setRevoked] = useState(false);
  const all = q.data?.credentials ?? [];
  const shown = revoked ? all : all.filter((c) => !c.revoked_at);
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_credentials()} />
      <HostCommand why={m.credentials_on_host()} command="yuuka token issue --principal KIND:NAME --scope SCOPE,SCOPE" />
      <label className="flex items-center gap-1.5 self-start text-small text-muted-foreground">
        <input type="checkbox" className="accent-current" checked={revoked} onChange={(e) => setRevoked(e.target.checked)} />
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
    </div>
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
        <span className="text-meta text-muted-foreground uppercase">{c.kind === "smtp_password" ? m.credentials_kind_smtp() : m.credentials_kind_token()}</span>
        {c.label && <span className="min-w-0 truncate">{c.label}</span>}
        {c.principal_disabled && <span className="text-meta text-warning">{m.credentials_principal_disabled()}</span>}
        {off && <span className="text-meta">{m.credentials_revoked({ when: listTime(c.revoked_at ?? "") })}</span>}
        <span className="ml-auto font-mono text-meta text-muted-foreground">{c.id}</span>
      </p>
      <p className="font-mono text-small wrap-anywhere">{c.scopes.join(" ")}</p>
      <Dates
        items={[
          [m.credentials_created(), listTime(c.created_at)],
          [m.credentials_expires(), c.expires_at && listTime(c.expires_at)],
          [m.credentials_last_used(), c.last_used_at ? listTime(c.last_used_at) : m.credentials_never_used()],
        ]}
      />
      {!off && (
        <div className="pt-0.5">
          <ConfirmButton
            label={m.credentials_revoke()}
            confirmLabel={m.credentials_revoke_confirm()}
            cost={c.kind === "smtp_password" ? m.credentials_revoke_cost_smtp({ who }) : m.credentials_revoke_cost({ who })}
            onConfirm={async () => {
              await api.POST("/api/v1/admin/credentials/{id}/revoke", { params: { path: { id: c.id } } });
              void qc.invalidateQueries({ queryKey: ["get", "/api/v1/admin/credentials"] });
            }}
          />
        </div>
      )}
    </li>
  );
}
