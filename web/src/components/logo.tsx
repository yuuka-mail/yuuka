import { cn } from "@/lib/utils";

/** The wordmark; the mark uses the system colour because yuuka is the system's voice. */
export function Logo({ className }: { className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-sm font-semibold tracking-tight", className)}>
      <span aria-hidden className="size-3 rounded-[3px] bg-system" />
      yuuka
    </span>
  );
}
