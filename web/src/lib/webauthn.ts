import { ApiError, api, data } from "@/api/client";
import type { ElevateResult, LoginResult, RegisterBegin, RegisterResult } from "@/api/types";
import { m } from "@/paraglide/messages.js";

// The WebAuthn ceremonies against internal/authn. The server speaks the
// JSON form of the options and credentials (binary fields as base64url),
// whose shape the server's WebAuthn library defines, not openapi.json;
// the browser API wants ArrayBuffers. PublicKeyCredential.parse*FromJSON
// and toJSON() do this natively, but not in every browser an operator's
// phone may run, so the few fields are converted by hand.

export function fromB64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
  const bin = atob(b64 + "===".slice((b64.length + 3) % 4));
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

export function toB64url(buf: ArrayBuffer | ArrayBufferView): string {
  const bytes =
    buf instanceof ArrayBuffer
      ? new Uint8Array(buf)
      : new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

interface JSONDescriptor {
  id: string;
  type: PublicKeyCredentialType;
  transports?: AuthenticatorTransport[];
}

interface JSONCreation {
  publicKey: Omit<
    PublicKeyCredentialCreationOptions,
    "challenge" | "user" | "excludeCredentials"
  > & {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: JSONDescriptor[];
  };
}

interface JSONRequest {
  publicKey: Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> & {
    challenge: string;
    allowCredentials?: JSONDescriptor[];
  };
}

const descriptor = (d: JSONDescriptor): PublicKeyCredentialDescriptor => ({
  ...d,
  id: fromB64url(d.id),
});

export function creationOptions(j: JSONCreation): PublicKeyCredentialCreationOptions {
  const pk = j.publicKey;
  return {
    ...pk,
    challenge: fromB64url(pk.challenge),
    user: { ...pk.user, id: fromB64url(pk.user.id) },
    excludeCredentials: pk.excludeCredentials?.map(descriptor),
  };
}

export function requestOptions(j: JSONRequest): PublicKeyCredentialRequestOptions {
  const pk = j.publicKey;
  return {
    ...pk,
    challenge: fromB64url(pk.challenge),
    allowCredentials: pk.allowCredentials?.map(descriptor),
  };
}

/** The JSON form of a credential, as the server parses it. */
export function credentialJSON(c: PublicKeyCredential) {
  const r = c.response;
  const response: Record<string, unknown> = { clientDataJSON: toB64url(r.clientDataJSON) };
  if (r instanceof AuthenticatorAttestationResponse) {
    response.attestationObject = toB64url(r.attestationObject);
    response.transports = r.getTransports?.() ?? [];
  } else if (r instanceof AuthenticatorAssertionResponse) {
    response.authenticatorData = toB64url(r.authenticatorData);
    response.signature = toB64url(r.signature);
    if (r.userHandle) response.userHandle = toB64url(r.userHandle);
  }
  return {
    id: c.id,
    rawId: toB64url(c.rawId),
    type: c.type,
    authenticatorAttachment: c.authenticatorAttachment ?? undefined,
    clientExtensionResults: c.getClientExtensionResults(),
    response,
  };
}

export function supported() {
  return (
    typeof window !== "undefined" && "PublicKeyCredential" in window && !!navigator.credentials
  );
}

/** Signs in with a discoverable passkey; the session arrives as a cookie. */
export async function login(): Promise<LoginResult> {
  const opts = (await data(
    api.POST("/auth/passkey/login/begin", { body: {} }),
  )) as unknown as JSONRequest;
  const cred = (await navigator.credentials.get({
    publicKey: requestOptions(opts),
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error(m.auth_no_passkey_chosen());
  return data(api.POST("/auth/passkey/login/finish", { body: credentialJSON(cred) }));
}

/**
 * Registers a new passkey, authorised by an enrollment token, or by the
 * current session when token is empty. It never logs in.
 */
export async function register(token: string, label: string): Promise<RegisterResult> {
  const body: RegisterBegin = { label };
  if (token) body.enrollment_token = token;
  const opts = (await data(
    api.POST("/auth/passkey/register/begin", { body }),
  )) as unknown as JSONCreation;
  const cred = (await navigator.credentials.create({
    publicKey: creationOptions(opts),
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error(m.auth_no_passkey_created());
  return data(api.POST("/auth/passkey/register/finish", { body: credentialJSON(cred) }));
}

/**
 * Asserts a passkey of the signed-in person again: for a few minutes the
 * session may widen who may do what (§13.10). It never logs in.
 */
export async function elevate(): Promise<ElevateResult> {
  const opts = (await data(
    api.POST("/auth/passkey/elevate/begin", { body: {} }),
  )) as unknown as JSONRequest;
  const cred = (await navigator.credentials.get({
    publicKey: requestOptions(opts),
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error(m.auth_no_passkey_chosen());
  return data(api.POST("/auth/passkey/elevate/finish", { body: credentialJSON(cred) }));
}

/** A passkey prompt that did not go through, said as ceremonyError says it. */
export class CeremonyError extends Error {
  readonly failure: Failure;
  constructor(failure: Failure) {
    super(failure.text);
    this.failure = failure;
  }
}

/**
 * Runs call, which widens who may do what. If the server answers that it
 * takes a fresh passkey assertion (elevation_required), asks for one and
 * runs call once more. The server decides; nothing here tracks the window.
 */
export async function withElevation<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call();
  } catch (err) {
    if (!(err instanceof ApiError) || err.code !== "elevation_required") throw err;
  }
  try {
    await elevate();
  } catch (err) {
    throw new CeremonyError(ceremonyError(err));
  }
  return call();
}

/**
 * A failed ceremony as people read it: a sentence of ours, and what the
 * browser or the server said only as a note (§12.11).
 */
export interface Failure {
  text: string;
  detail?: string;
}

/** A readable message for a failed ceremony. */
export function ceremonyError(err: unknown): Failure {
  if (err instanceof DOMException) {
    if (err.name === "NotAllowedError") return { text: m.auth_err_not_allowed() };
    if (err.name === "InvalidStateError") return { text: m.auth_err_invalid_state() };
    if (err.name === "SecurityError") return { text: m.auth_err_security() };
    return { text: m.auth_err_ceremony(), detail: err.message };
  }
  if (err instanceof ApiError) {
    if (err.status === 429) return { text: m.auth_err_too_many() };
    return { text: m.auth_err_refused(), detail: err.message };
  }
  // fetch fails with a TypeError when the server cannot be reached.
  if (err instanceof TypeError) return { text: m.auth_err_unreachable(), detail: err.message };
  // Errors thrown above carry our own words already.
  return { text: err instanceof Error ? err.message : String(err) };
}
