import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { useQueryClient } from "@tanstack/react-query";
import { KeyRoundIcon } from "lucide-react";
import { useState } from "react";

import { markup } from "@/components/markup";
import { Button } from "@/components/ui/button";
import { navigate, useLocation } from "@/lib/router";
import { ceremonyError, login, supported, type Failure } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";

import { AuthLayout, ErrorText, FailureText } from "./auth-layout";

/**
 * Passkey login, the only way to get a session (§13.7). There is
 * deliberately no link to recovery here: recovery codes are never part of
 * the everyday login path, so there is nothing on it to phish.
 */
export function LoginPage({ error }: { error?: unknown }) {
  const qc = useQueryClient();
  const { pathname } = useLocation();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<Failure | undefined>(
    error ? ceremonyError(error) : undefined,
  );

  async function signIn() {
    setBusy(true);
    setFailure(undefined);
    try {
      await login();
      await qc.resetQueries();
      if (pathname === "/login") navigate("/", { replace: true });
    } catch (err) {
      setFailure(ceremonyError(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthLayout title={m.auth_sign_in()}>
      <p className="text-body text-muted-foreground">{m.auth_login_hint()}</p>
      <Button size="lg" onClick={signIn} disabled={busy || !supported()} className="self-start">
        <KeyRoundIcon data-icon="inline-start" />
        {busy ? m.auth_waiting_passkey() : m.auth_sign_in_passkey()}
      </Button>
      {!supported() && <ErrorText>{m.auth_unsupported()}</ErrorText>}
      <FailureText f={failure} />
      <p className="text-small text-muted-foreground">
        <ParaglideMessage message={m.auth_login_no_passkey} markup={markup} />
      </p>
    </AuthLayout>
  );
}
