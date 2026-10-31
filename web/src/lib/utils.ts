import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

// The type scale's roles (index.css, §12.8) are font sizes. Unknown to
// tailwind-merge, text-ui would read as a colour and silently drop the
// text colour beside it (a primary button's text-primary-foreground).
const twMerge = extendTailwindMerge({
  extend: { classGroups: { "font-size": [{ text: ["meta", "small", "ui", "body", "title"] }] } },
});

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
