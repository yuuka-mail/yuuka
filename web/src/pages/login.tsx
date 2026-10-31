import { useQueryClient } from "@tanstack/react-query";
import { KeyRoundIcon } from "lucide-react";
import { useState } from "react";

import { ApiError } from "@/api/client";
import { Button } from "@/components/ui/button";
import { navigate, useLocation } from "@/lib/router";
import { ceremonyError, login, supported } from "@/lib/webauthn";

import { AuthLayout, ErrorText } from "./auth-layout";

/**
 * Passkey login, the only way to get a session (§13.7). There is
 * deliberately no link to recovery here: recovery codes are never part of
 * the everyday login path, so there is nothing on it to phish.
 */
export function LoginPage({ error }: { error?: unknown }) {
  const qc = useQueryClient();
  const { pathname } = useLocation();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState(error ? errorText(error) : "");

  async function signIn() {
    setBusy(true);
    setFailure("");
    try {
      await login();
      await qc.resetQueries();
      if (pathname === "/login") navigate("/", { replace: true });
    } catch (err) {
      setFailure(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthLayout title="Sign in">
      <p className="text-body text-muted-foreground">Use a passkey registered for this mailbox.</p>
      <Button size="lg" onClick={signIn} disabled={busy || !supported()} className="self-start">
        <KeyRoundIcon data-icon="inline-start" />
        {busy ? "Waiting for passkey…" : "Sign in with passkey"}
      </Button>
      {!supported() && <ErrorText>This browser does not support passkeys.</ErrorText>}
      <ErrorText>{failure}</ErrorText>
      <p className="text-small text-muted-foreground">
        No passkey yet? Run <code className="font-mono">yuuka admin enroll</code> on the server for a one-time link.
      </p>
    </AuthLayout>
  );
}

function errorText(err: unknown) {
  if (err instanceof ApiError && err.status === 429) return "Too many attempts. Wait a moment and try again.";
  return ceremonyError(err);
}
