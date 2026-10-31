import { KeyRoundIcon } from "lucide-react";
import { useState } from "react";

import { useSession } from "@/api/queries";
import { system, Voice } from "@/components/voice";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ceremonyError, register, supported } from "@/lib/webauthn";

import { AuthLayout, ErrorText } from "./auth-layout";

// The token travels in the fragment so that it never reaches a server or
// proxy log (§13.7). It is moved out of the address bar on first read, so
// it does not linger in history either; repeated reads (StrictMode runs
// state initialisers twice) get the same value.
let lastToken = "";
function takeToken(): string {
  const fromURL = decodeURIComponent(window.location.hash.slice(1));
  if (fromURL) {
    lastToken = fromURL;
    window.history.replaceState(null, "", window.location.pathname);
  }
  return lastToken;
}

/**
 * Registers a passkey with a one-time link from `yuuka admin enroll` or
 * from a recovery code, or, without a link, adds one to the signed-in
 * operator. It never signs in: the new passkey does that afterwards.
 */
export function EnrollPage() {
  const [token] = useState(takeToken);
  const session = useSession();
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");
  const [done, setDone] = useState(false);

  const viaSession = !token && !!session.data;
  const usable = !!token || viaSession;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setFailure("");
    try {
      await register(token, label.trim());
      setDone(true);
    } catch (err) {
      setFailure(ceremonyError(err));
    } finally {
      setBusy(false);
    }
  }

  if (done) {
    return (
      <AuthLayout title="Passkey registered">
        <Voice speaker={system}>
          <p className="text-sm text-foreground">
            The passkey is stored. {viaSession ? "It works from the next sign-in." : "The link is used up; sign in with the new passkey."}
          </p>
        </Voice>
        <Button render={<a href={viaSession ? "/" : "/login"} />} nativeButton={false} className="self-start">
          {viaSession ? "Back to mail" : "Sign in"}
        </Button>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={viaSession ? "Add a passkey" : "Register a passkey"}>
      {!usable && !session.isPending ? (
        <ErrorText>
          This page needs a one-time enrollment link. Run <code className="font-mono">yuuka admin enroll</code> on the server, or use a recovery code at{" "}
          <a className="underline" href="/recover">
            /recover
          </a>
          .
        </ErrorText>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">
            {viaSession
              ? `Adds another passkey for ${session.data?.principal}.`
              : "This link registers one passkey and then expires. Registering does not sign you in."}
          </p>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="font-medium">Name for this passkey</span>
            <Input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. laptop, phone" maxLength={100} />
          </label>
          <Button type="submit" size="lg" disabled={busy || !supported()} className="self-start">
            <KeyRoundIcon data-icon="inline-start" />
            {busy ? "Waiting for authenticator…" : "Register passkey"}
          </Button>
          {!supported() && <ErrorText>This browser does not support passkeys.</ErrorText>}
          <ErrorText>{failure}</ErrorText>
        </form>
      )}
    </AuthLayout>
  );
}
