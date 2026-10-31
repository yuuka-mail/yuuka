// Email Worker: persist the envelope and the raw message to R2, then nudge
// yuuka with a signed webhook. It deliberately knows nothing about MIME; see
// docs/design/architecture.md §2.1 and §2.2 for the protocol this implements.

import {
  type Marker,
  MARKER_METADATA_KEY,
  base64url,
  pendingKey,
  rawKey,
  serializeMarker,
  sign,
} from "./contract";
import { ulid } from "./ulid";

export interface Env {
  RAW: R2Bucket;
  PENDING: R2Bucket;
  INGEST_URL: string;
  WEBHOOK_SECRET: string;
}

const WRITE_DEADLINE_MS = 60_000;
const WEBHOOK_TIMEOUT_MS = 5_000;

export default {
  async email(message, env, ctx) {
    await ingest(message, env, ctx);
  },
} satisfies ExportedHandler<Env>;

// ingest throws on any storage failure so Cloudflare answers 421 and the
// sender retries (§2.1.1). It never calls setReject(): a 5xx would bounce the
// message, i.e. lose it. The marker is returned for tests.
export async function ingest(
  message: ForwardableEmailMessage,
  env: Env,
  ctx: ExecutionContext,
  deadlineMs = WRITE_DEADLINE_MS,
): Promise<Marker> {
  const now = Date.now();
  const id = ulid(now);
  const receivedAt = new Date(now);

  // put() needs a known length, and raw_size must be what was actually
  // written rather than the declared message.rawSize.
  const raw = await new Response(message.raw).arrayBuffer();

  const marker: Marker = {
    v: 1,
    id,
    received_at: receivedAt.toISOString(),
    envelope_from: nullSender(message.from) ? "" : message.from,
    envelope_to: message.to,
    raw_key: rawKey(id, receivedAt),
    raw_size: raw.byteLength,
  };
  const body = new TextEncoder().encode(serializeMarker(marker));

  try {
    await withDeadline(store(env, marker, body, raw), deadlineMs);
  } catch (err) {
    log("error", { id, raw_size: marker.raw_size, step: "store", error: describe(err) });
    throw err;
  }
  log("info", { id, raw_size: marker.raw_size, step: "store", ok: true });

  ctx.waitUntil(notify(env, id, body));
  return marker;
}

// What message.from holds for MAIL FROM:<> is an open M0 question (§2.2),
// so accept both spellings.
function nullSender(from: string): boolean {
  return !from || from === "<>";
}

// The marker goes first: if the raw write then fails, reconcile finds an
// orphan marker and drops it; the reverse order could leave a raw object
// nobody is told about (§2.1).
async function store(env: Env, marker: Marker, body: Uint8Array, raw: ArrayBuffer): Promise<void> {
  await env.PENDING.put(pendingKey(marker.id), body);
  // Create-only. The Headers form is used because the "*" wildcard is
  // defined by HTTP (RFC 9110 If-None-Match), whereas whether the object
  // form's etagDoesNotMatch treats "*" as a wildcard is not documented.
  const obj = await env.RAW.put(marker.raw_key, raw, {
    customMetadata: { [MARKER_METADATA_KEY]: base64url(body) },
    onlyIf: new Headers({ "If-None-Match": "*" }),
  });
  if (obj === null) {
    throw new Error(`raw object ${marker.raw_key} already exists`);
  }
}

// R2 put() cannot be cancelled, so the deadline only stops waiting. A write
// that lands afterwards leaves a complete marker + raw pair, which reconcile
// ingests and retry dedup merges with the sender's retry (§2.2 step 5).
async function withDeadline<T>(work: Promise<T>, ms: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | null = null;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`R2 writes exceeded ${ms} ms`)), ms);
  });
  try {
    return await Promise.race([work, deadline]);
  } finally {
    clearTimeout(timer);
  }
}

// Best effort: correctness comes from reconcile, so the outcome is only
// logged, never retried or thrown.
async function notify(env: Env, id: string, body: Uint8Array): Promise<void> {
  try {
    const timestamp = Math.floor(Date.now() / 1000);
    const res = await fetch(env.INGEST_URL, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Yuuka-Timestamp": String(timestamp),
        "Yuuka-Signature": await sign(env.WEBHOOK_SECRET, timestamp, body),
      },
      body,
      signal: AbortSignal.timeout(WEBHOOK_TIMEOUT_MS),
    });
    await res.body?.cancel();
    log(res.ok ? "info" : "warn", { id, step: "webhook", status: res.status });
  } catch (err) {
    log("warn", { id, step: "webhook", error: describe(err) });
  }
}

// Logs live on Cloudflare's side, outside the operator's control: only ids,
// sizes and outcomes, never addresses, subjects or bodies.
type LogFields = { id: string; step: string; raw_size?: number; ok?: boolean; status?: number; error?: string };

function log(level: "info" | "warn" | "error", fields: LogFields): void {
  console[level](JSON.stringify(fields));
}

function describe(err: unknown): string {
  return err instanceof Error ? `${err.name}: ${err.message}` : String(err);
}
