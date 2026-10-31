// The Worker side of the Worker <-> Go contract v1 (docs/architecture.md §2.2).
// Any change here is a one-way door: it must land together with the Go side
// and the shared vectors in testdata/contract/.

export interface Marker {
  v: 1;
  id: string;
  received_at: string;
  envelope_from: string;
  envelope_to: string;
  raw_key: string;
  raw_size: number;
}

// The pending object and the webhook body are the same bytes, and Go verifies
// the HMAC over the bytes it receives, so the output must be deterministic:
// fields in contract order, no whitespace. The object is rebuilt so the
// caller's key order cannot leak into the wire format.
export function serializeMarker(m: Marker): string {
  return JSON.stringify({
    v: m.v,
    id: m.id,
    received_at: m.received_at,
    envelope_from: m.envelope_from,
    envelope_to: m.envelope_to,
    raw_key: m.raw_key,
    raw_size: m.raw_size,
  });
}

export function rawKey(id: string, receivedAt: Date): string {
  const month = String(receivedAt.getUTCMonth() + 1).padStart(2, "0");
  return `raw/${receivedAt.getUTCFullYear()}/${month}/${id}.eml`;
}

export function pendingKey(id: string): string {
  return `pending/${id}`;
}

export const MARKER_METADATA_KEY = "yuuka-marker";

// R2 mangles non-ASCII metadata values on the S3 read path (RFC 2047), so the
// marker travels as base64url without padding.
export function base64url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

const encoder = new TextEncoder();

// Signs "v1:" + timestamp + ":" + body, where body is the exact bytes sent.
export async function sign(secret: string, timestamp: number, body: Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const prefix = encoder.encode(`v1:${timestamp}:`);
  const input = new Uint8Array(prefix.length + body.length);
  input.set(prefix);
  input.set(body, prefix.length);
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, input));
  return "v1=" + Array.from(mac, (b) => b.toString(16).padStart(2, "0")).join("");
}
