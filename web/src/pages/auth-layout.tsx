import type { ReactNode } from "react";

import { Logo } from "@/components/logo";

/** The narrow column the login, enrollment and recovery pages share. */
export function AuthLayout({ title, children }: { title: string; children: ReactNode }) {
  return (
    <main className="flex min-h-full items-start justify-center px-4 pt-[18vh] pb-8">
      <div className="flex w-full max-w-sm flex-col gap-5">
        <Logo />
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {children}
      </div>
    </main>
  );
}

export function ErrorText({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <p role="alert" className="text-sm text-destructive">
      {children}
    </p>
  );
}
