import horizontal from "@/brand/horizontal-color.svg";
import horizontalReverse from "@/brand/horizontal-reverse.svg";
import { cn } from "@/lib/utils";

/**
 * The horizontal lockup, scaled whole (§14.5). The picture picks the variant
 * so it follows the same OS preference as the colour tokens: the navy outline
 * of the colour version disappears on a dark background.
 */
export function Logo({ className }: { className?: string }) {
  return (
    <picture className={cn("inline-flex h-9 shrink-0 items-center", className)}>
      <source media="(prefers-color-scheme: dark)" srcSet={horizontalReverse} />
      <img src={horizontal} alt="yuuka" className="h-full w-auto max-w-none" />
    </picture>
  );
}
