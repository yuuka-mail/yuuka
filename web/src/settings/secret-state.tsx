import { $api } from "@/api/client";
import type { SecretStatus } from "@/api/types";
import { Mark, Signature, system } from "@/components/voice";
import { fullTime, listTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";

import { secretState } from "./thirdparty";

// How a sealed secret is shown (§17.6): that one is set, when it was
// sealed, and whether it opens. Nothing else about it exists here.

/** "password · sealed 3 Nov · opens" in one line; yuuka's mark, it is the system's fact. */
export function SecretLine({ label, s }: { label: string; s: SecretStatus }) {
  const st = secretState(s);
  return (
    <p className="flex flex-wrap items-center gap-x-2 text-small">
      <Mark kind="system" />
      <span>{label}</span>
      <span className="text-muted-foreground tabular-nums" title={fullTime(s.sealed_at)}>
        {m.secret_sealed({ when: listTime(s.sealed_at) })}
      </span>
      <span className={cn("font-medium", st.ok ? "text-system-foreground" : "text-warning")}>
        {st.text}
      </span>
    </p>
  );
}

/** The locked state where a section's writes would need the key. */
export function LockedNote({ text }: { text: string }) {
  return (
    <p className="flex flex-wrap items-baseline gap-x-2 rounded-md border border-system-border bg-system-surface px-2 py-1.5 text-small">
      <Signature speaker={system} />
      <span>{text}</span>
    </p>
  );
}

/**
 * The banner at the top of the WebUI while the server has no key for
 * third-party secrets (§17.3): mail still arrives, but transports with a
 * password hold their mail and no notification channel runs. Only a
 * person's session with admin can ask (the route is theirs), and that is
 * everyone who signs in to the WebUI today.
 */
export function LockedBanner({ enabled, onOpen }: { enabled: boolean; onOpen: () => void }) {
  const q = $api.useQuery(
    "get",
    "/api/v1/admin/secrets",
    {},
    { enabled, staleTime: 60_000, retry: false },
  );
  if (!q.data?.locked) return null;
  return (
    <div
      role="status"
      className="flex shrink-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 border-b border-system-border bg-system-surface px-3 py-1.5 text-small"
    >
      <Signature speaker={system} />
      <span className="font-medium">{m.locked_banner_title()}</span>
      <span className="text-muted-foreground">{m.locked_banner_body()}</span>
      <button
        type="button"
        onClick={onOpen}
        className="text-system-foreground underline-offset-2 hover:underline"
      >
        {m.locked_banner_open()}
      </button>
    </div>
  );
}
