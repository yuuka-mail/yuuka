import { useQueryClient } from "@tanstack/react-query";

import { $api, api } from "@/api/client";
import type { ManagedIdentity } from "@/api/types";
import { HostCommand } from "@/components/cli";
import { ConfirmButton } from "@/components/confirm";
import { Skeleton } from "@/components/ui/skeleton";
import { listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { ProblemLine } from "@/send/marks";
import { principal, problem } from "@/send/outgoing";

import { Dates, SectionHeader } from "./section";

// Identities (§9.3, §13.10): the addresses yuuka sends as. Disabling one
// is a tightening, except where the zone's *@zone pattern would take the
// address over with its own owner and grants: that is done on the host,
// and the page says the command.

export function IdentitiesSection() {
  const q = $api.useQuery("get", "/api/v1/admin/identities", {});
  return (
    <div className="flex max-w-[64rem] flex-col gap-3 px-4 py-4 md:px-6">
      <SectionHeader title={m.settings_identities()} />
      <HostCommand why={m.identities_on_host()} command="yuuka identity add LOCAL@ZONE --owner KIND:NAME --transport NAME" />
      {q.error ? (
        <ProblemLine p={problem(q.error)} />
      ) : !q.data ? (
        <Skeleton className="h-24 w-full" />
      ) : q.data.identities.length === 0 ? (
        <p className="text-muted-foreground">{m.identities_none()}</p>
      ) : (
        <ul aria-label={m.settings_identities()} className="flex flex-col border-t">
          {q.data.identities.map((i) => (
            <IdentityRow key={i.id} i={i} />
          ))}
        </ul>
      )}
    </div>
  );
}

function IdentityRow({ i }: { i: ManagedIdentity }) {
  const qc = useQueryClient();
  const off = !!i.disabled_at;
  return (
    <li className={cn("flex flex-col gap-1 border-b px-2 py-2", off && "text-muted-foreground")}>
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-ui">
        <span className="font-mono font-medium">{i.address}</span>
        {i.display_name && <span className="min-w-0 truncate">{i.display_name}</span>}
        {off && <span className="text-meta">{m.identities_disabled({ when: listTime(i.disabled_at ?? "") })}</span>}
      </p>
      <Dates
        items={[
          [m.identities_owner(), principal(i.owner)],
          [m.identities_transport(), i.transport],
          [m.credentials_created(), listTime(i.created_at)],
        ]}
      />
      {!off &&
        (i.taken_over_by ? (
          <details className="text-small">
            <summary className="cursor-pointer text-muted-foreground hover:text-foreground">{m.identities_disable_on_host()}</summary>
            <HostCommand className="mt-1" why={m.identities_taken_over({ pattern: i.taken_over_by })} command={i.host} />
          </details>
        ) : (
          <div className="pt-0.5">
            <ConfirmButton
              label={m.identities_disable()}
              confirmLabel={m.identities_disable_confirm()}
              cost={m.identities_disable_cost({ address: i.address })}
              onConfirm={async () => {
                await api.POST("/api/v1/admin/identities/{id}/disable", { params: { path: { id: i.id } } });
                for (const k of ["/api/v1/admin/identities", "/api/v1/admin/grants", "/api/v1/identities"]) {
                  void qc.invalidateQueries({ queryKey: ["get", k] });
                }
              }}
            />
          </div>
        ))}
    </li>
  );
}
