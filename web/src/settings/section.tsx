import type { ReactNode } from "react";

/** The heading of a settings section, with room for a control at its right. */
export function SectionHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <header className="flex flex-wrap items-center gap-2">
      <h2 className="text-title font-semibold">{title}</h2>
      {children && <span className="ml-auto flex items-center gap-2">{children}</span>}
    </header>
  );
}

/** "label value" pairs of a row's second line, the value in the meta size. */
export function Dates({ items }: { items: [string, string | undefined][] }) {
  return (
    <span className="flex flex-wrap gap-x-3 text-meta text-muted-foreground tabular-nums">
      {items
        .filter(([, v]) => v)
        .map(([k, v]) => (
          <span key={k}>
            {k} {v}
          </span>
        ))}
    </span>
  );
}
