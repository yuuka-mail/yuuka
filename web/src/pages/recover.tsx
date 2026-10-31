import { useState } from "react";

import type { RecoveryGrant } from "@/api/auth.gen";
import { ApiError, postJSON } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { navigate } from "@/lib/router";

import { AuthLayout, ErrorText } from "./auth-layout";

/**
 * Exchanges an offline recovery code for a one-time enrollment token and
 * goes straight on to registering a passkey (§13.7). The code never signs
 * in; it can only start a registration.
 */
export function RecoverPage() {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setFailure("");
    try {
      const g = await postJSON<RecoveryGrant>("/auth/recovery", { code: code.trim() });
      navigate(`/enroll#${encodeURIComponent(g.enrollment_token)}`, { replace: true });
    } catch (err) {
      setFailure(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthLayout title="Recover access">
      <p className="text-sm text-muted-foreground">
        A recovery code lets you register one new passkey. It does not sign you in, works once, and the operator is alerted when it is used.
      </p>
      <form onSubmit={submit} className="flex flex-col gap-4">
        <label className="flex flex-col gap-1.5 text-sm">
          <span className="font-medium">Recovery code</span>
          <Input value={code} onChange={(e) => setCode(e.target.value)} autoComplete="off" spellCheck={false} className="font-mono" required />
        </label>
        <Button type="submit" size="lg" disabled={busy || !code.trim()} className="self-start">
          {busy ? "Checking…" : "Continue to passkey registration"}
        </Button>
        <ErrorText>{failure}</ErrorText>
      </form>
    </AuthLayout>
  );
}
