import type { ReactNode } from "react";

// How the markup in messages renders (§12.11): the translator decides
// where in the sentence {#code}…{/code} sits, these decide what it looks
// like. Pass them as ParaglideMessage's markup; a message uses only the
// tags it names.

type Props = { children?: ReactNode };

function Code({ children }: Props) {
  return <code className="font-mono">{children}</code>;
}

function Strong({ children }: Props) {
  return <span className="font-medium">{children}</span>;
}

function Key({ children }: Props) {
  return <kbd className="rounded border bg-muted px-1 font-mono text-meta">{children}</kbd>;
}

function Mono({ children }: Props) {
  return <span className="font-mono">{children}</span>;
}

/**
 * Words a sender wrote (a subject), in no known language: the UI's
 * language must not pick their glyphs (§12.11).
 */
function Mail({ children }: Props) {
  return (
    <span lang="" className="wrap-anywhere">
      {children}
    </span>
  );
}

function RecoverLink({ children }: Props) {
  return (
    <a className="underline" href="/recover">
      {children}
    </a>
  );
}

export const markup = {
  code: Code,
  b: Strong,
  k: Key,
  mono: Mono,
  mail: Mail,
  recover: RecoverLink,
};
