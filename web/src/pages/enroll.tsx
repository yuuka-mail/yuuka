import { ParaglideMessage } from "@inlang/paraglide-js-react";
import { KeyRoundIcon } from "lucide-react";
import { useState } from "react";

import { markup } from "@/components/markup";
import { useSession } from "@/api/queries";
import { system, Voice } from "@/components/voice";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ceremonyError, register, supported, type Failure } from "@/lib/webauthn";
import { m } from "@/paraglide/messages.js";

import { AuthLayout, ErrorText, FailureText } from "./auth-layout";

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
  const [failure, setFailure] = useState<Failure>();
  const [done, setDone] = useState(false);

  const viaSession = !token && !!session.data;
  const usable = !!token || viaSession;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setFailure(undefined);
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
      <AuthLayout title={m.auth_enroll_done_title()}>
        <Voice speaker={system}>
          <p className="text-body text-foreground">
            {viaSession ? m.auth_enroll_done_session() : m.auth_enroll_done_link()}
          </p>
        </Voice>
        <Button
          render={<a href={viaSession ? "/" : "/login"} />}
          nativeButton={false}
          className="self-start"
        >
          {viaSession ? m.auth_back_to_mail() : m.auth_sign_in()}
        </Button>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={viaSession ? m.auth_enroll_add_title() : m.auth_enroll_title()}>
      {!usable && !session.isPending ? (
        <ErrorText>
          <ParaglideMessage message={m.auth_enroll_needs_link} markup={markup} />
        </ErrorText>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-4">
          <p className="text-body text-muted-foreground">
            {viaSession
              ? m.auth_enroll_adds_for({ principal: session.data?.principal ?? "" })
              : m.auth_enroll_link_once()}
          </p>
          <label className="flex flex-col gap-1.5 text-body">
            <span className="font-medium">{m.auth_passkey_name()}</span>
            <Input
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder={m.auth_passkey_name_hint()}
              maxLength={100}
            />
          </label>
          <Button type="submit" size="lg" disabled={busy || !supported()} className="self-start">
            <KeyRoundIcon data-icon="inline-start" />
            {busy ? m.auth_waiting_authenticator() : m.auth_register_passkey()}
          </Button>
          {!supported() && <ErrorText>{m.auth_unsupported()}</ErrorText>}
          <FailureText f={failure} />
        </form>
      )}
    </AuthLayout>
  );
}
