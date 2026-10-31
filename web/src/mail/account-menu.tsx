import { useQueryClient } from "@tanstack/react-query";
import { ChevronDownIcon, LogOutIcon, SettingsIcon } from "lucide-react";

import { api } from "@/api/client";
import { keys } from "@/api/queries";
import type { SessionInfo } from "@/api/types";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";
import { m } from "@/paraglide/messages.js";
import { getLocale, locales, setLocale, type Locale } from "@/paraglide/runtime.js";

/** Each language by its own name, so it can be found whatever is shown now. */
const autonyms: Record<Locale, string> = { en: "English", zh: "中文" };

/**
 * The top bar's account menu: who is signed in, the settings page
 * (§12.12), the UI language (kept in this browser, §12.11) and signing
 * out. Changing the language reloads the page; a draft being written is
 * saved as it is typed (§12.10).
 */
export function AccountMenu({ session, className, onSettings }: { session: SessionInfo; className?: string; onSettings?: () => void }) {
  const qc = useQueryClient();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        aria-label={m.account_menu()}
        className={cn(
          "inline-flex h-7 max-w-56 min-w-0 items-center gap-1 rounded-md px-2 text-meta text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
          className,
        )}
      >
        <span className="truncate max-sm:sr-only">{session.principal}</span>
        <ChevronDownIcon aria-hidden className="size-3 shrink-0" />
      </DropdownMenuTrigger>
      <DropdownMenuContent>
        <DropdownMenuGroup>
          <DropdownMenuLabel className="truncate font-mono">{session.principal}</DropdownMenuLabel>
        </DropdownMenuGroup>
        {onSettings && (
          <DropdownMenuItem onClick={onSettings}>
            <SettingsIcon aria-hidden />
            {m.account_settings()}
          </DropdownMenuItem>
        )}
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuLabel>{m.account_language()}</DropdownMenuLabel>
          <DropdownMenuRadioGroup value={getLocale()} onValueChange={(l: Locale) => void setLocale(l)}>
            {locales.map((l) => (
              <DropdownMenuRadioItem key={l} value={l} lang={l}>
                {autonyms[l]}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onClick={async () => {
            await api.POST("/auth/logout").catch(() => undefined);
            qc.clear();
            qc.setQueryData(keys.session, null);
          }}
        >
          <LogOutIcon aria-hidden />
          {m.account_sign_out()}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
