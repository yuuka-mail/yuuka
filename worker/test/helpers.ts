// The Worker never decodes the metadata (Go does); tests use this to check
// the round trip.
export function decodeBase64url(s: string): string {
  const bin = atob(s.replaceAll("-", "+").replaceAll("_", "/"));
  return new TextDecoder().decode(Uint8Array.from(bin, (ch) => ch.charCodeAt(0)));
}
