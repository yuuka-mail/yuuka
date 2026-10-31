import { useState } from "react";

import { ApiError, api, data } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { navigate } from "@/lib/router";
import type { Failure } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";

import { AuthLayout, FailureText } from "./auth-layout";

/**
 * A failed exchange in our words, chosen by status: the server's text is
 * only a note (§12.11).
 */
function recoverError(err: unknown): Failure {
  if (err instanceof ApiError) {
    if (err.status === 429) return { text: m.auth_err_too_many() };
    return { text: m.auth_recover_bad_code(), detail: err.message };
  }
  return {
    text: m.auth_err_unreachable(),
    detail: err instanceof Error ? err.message : String(err),
  };
}

/**
 * Exchanges an offline recovery code for a one-time enrollment token and
 * goes straight on to registering a passkey (§13.7). The code never signs
 * in; it can only start a registration.
 */
export function RecoverPage() {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<Failure>();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setFailure(undefined);
    try {
      const g = await data(api.POST("/auth/recovery", { body: { code: code.trim() } }));
      navigate(`/enroll#${encodeURIComponent(g.enrollment_token)}`, { replace: true });
    } catch (err) {
      setFailure(recoverError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthLayout title={m.auth_recover_title()}>
      <p className="text-body text-muted-foreground">{m.auth_recover_hint()}</p>
      <form onSubmit={submit} className="flex flex-col gap-4">
        <label className="flex flex-col gap-1.5 text-body">
          <span className="font-medium">{m.auth_recover_code()}</span>
          <Input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            required
          />
        </label>
        <Button type="submit" size="lg" disabled={busy || !code.trim()} className="self-start">
          {busy ? m.auth_recover_checking() : m.auth_recover_continue()}
        </Button>
        <FailureText f={failure} />
      </form>
    </AuthLayout>
  );
}
