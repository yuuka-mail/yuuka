import { m } from "@/paraglide/messages.js";

import type { Area } from "./view";

/** What the sidebar and the list header call an area. */
export function areaTitle(a: Area): string {
  switch (a) {
    case "conversations":
      return m.area_conversations();
    case "notifications":
      return m.area_notifications();
    case "all":
      return m.area_all();
    case "drafts":
      return m.area_drafts();
    case "outbox":
      return m.area_outbox();
    case "downgraded":
      return m.area_downgraded();
    case "unclaimed":
      return m.area_unclaimed();
    case "leaks":
      return m.area_leaks();
  }
}
