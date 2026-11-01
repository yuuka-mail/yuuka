import type { ReactNode } from "react";

import { Logo } from "@/components/logo";
import type { Failure } from "@/lib/webauthn";

/** The narrow column the login, enrollment and recovery pages share. */
export function AuthLayout({ title, children }: { title: string; children: ReactNode }) {
  return (
    <main className="flex min-h-full items-start justify-center px-4 pt-[18vh] pb-8">
      <div className="flex w-full max-w-sm flex-col gap-5">
        <Logo className="-ml-1.5 h-12 self-start" />
        <h1 className="text-title font-semibold tracking-tight">{title}</h1>
        {children}
      </div>
    </main>
  );
}

export function ErrorText({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <p role="alert" className="text-body text-destructive">
      {children}
    </p>
  );
}

/** A failure in our words, with what the browser or server said as a note (§12.11). */
export function FailureText({ f }: { f?: Failure }) {
  if (!f) return null;
  return (
    <ErrorText>
      {f.text}
      {f.detail && <span className="text-small text-foreground/80"> · {f.detail}</span>}
    </ErrorText>
  );
}
