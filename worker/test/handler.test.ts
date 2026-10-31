import { createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it, vi } from "vitest";
import hmac from "../../testdata/contract/v1/hmac.json";
import { pendingKey, serializeMarker } from "../src/contract";
import worker, { type Env, ingest } from "../src/index";
import { decodeBase64url } from "./helpers";

const RAW = new TextEncoder().encode(
  "From: Alice <alice@example.net>\r\nTo: github@example.com\r\nSubject: hi\r\n\r\nhello\r\n",
);

function message(from = "alice@example.net", to = "github@example.com", raw = RAW) {
  return {
    from,
    to,
    raw: new Response(raw).body!,
    rawSize: raw.byteLength,
    headers: new Headers(),
    setReject: vi.fn(),
    forward: vi.fn(),
    reply: vi.fn(),
  } satisfies Partial<ForwardableEmailMessage> as unknown as ForwardableEmailMessage & {
    setReject: ReturnType<typeof vi.fn>;
  };
}

function mockWebhook(impl: () => Promise<Response> = async () => new Response(null, { status: 204 })) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(impl);
}

// A bucket whose put() is replaced, for failure paths the emulator cannot produce.
function bucket(put: () => Promise<R2Object | null>): R2Bucket {
  return { put } as unknown as R2Bucket;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ingest", () => {
  it("writes the marker and the raw object, then signs the webhook", async () => {
    const fetch = mockWebhook();
    const msg = message();
    const ctx = createExecutionContext();

    const marker = await ingest(msg, env, ctx);
    await waitOnExecutionContext(ctx);

    const json = serializeMarker(marker);
    expect(marker.raw_size).toBe(RAW.byteLength);
    expect(marker.envelope_from).toBe("alice@example.net");
    expect(marker.envelope_to).toBe("github@example.com");

    const pending = await env.PENDING.get(pendingKey(marker.id));
    expect(await pending!.text()).toBe(json);

    const raw = await env.RAW.get(marker.raw_key);
    expect(new Uint8Array(await raw!.arrayBuffer())).toEqual(RAW);
    expect(decodeBase64url(raw!.customMetadata!["yuuka-marker"]!)).toBe(json);

    expect(fetch).toHaveBeenCalledOnce();
    const req = new Request(...(fetch.mock.calls[0] as [RequestInfo, RequestInit]));
    expect(req.url).toBe(env.INGEST_URL);
    expect(req.method).toBe("POST");
    expect(req.headers.get("Content-Type")).toBe("application/json");
    const body = await req.text();
    expect(body).toBe(json);
    const timestamp = Number(req.headers.get("Yuuka-Timestamp"));
    expect(Math.abs(timestamp - Date.now() / 1000)).toBeLessThanOrEqual(hmac.window_seconds);
    expect(await verify(env.WEBHOOK_SECRET, timestamp, body, req.headers.get("Yuuka-Signature")!)).toBe(true);

    expect(msg.setReject).not.toHaveBeenCalled();
  });

  it.each(["", "<>"])("records null sender %j as an empty string", async (from) => {
    mockWebhook();
    const marker = await ingest(message(from), env, createExecutionContext());
    expect(marker.envelope_from).toBe("");
    const pending = await env.PENDING.get(pendingKey(marker.id));
    expect(JSON.parse(await pending!.text()).envelope_from).toBe("");
  });

  it("throws when the pending write fails, without writing raw", async () => {
    const rawPut = vi.fn(async () => null);
    const broken: Env = {
      ...env,
      PENDING: bucket(async () => {
        throw new Error("r2 unavailable");
      }),
      RAW: bucket(rawPut),
    };
    await expect(ingest(message(), broken, createExecutionContext())).rejects.toThrow("r2 unavailable");
    expect(rawPut).not.toHaveBeenCalled();
  });

  it("throws when the raw write fails", async () => {
    const broken: Env = {
      ...env,
      RAW: bucket(async () => {
        throw new Error("r2 unavailable");
      }),
    };
    await expect(ingest(message(), broken, createExecutionContext())).rejects.toThrow("r2 unavailable");
  });

  it("throws when the writes miss the deadline", async () => {
    const fetch = mockWebhook();
    const slow: Env = { ...env, RAW: bucket(() => new Promise((r) => setTimeout(() => r(null), 1_000))) };
    await expect(ingest(message(), slow, createExecutionContext(), 20)).rejects.toThrow(/exceeded 20 ms/);
    expect(fetch).not.toHaveBeenCalled();
  });

  // Note: miniflare has reported reversed etag conditions (cloudflare/miniflare#774).
  // If this fails only under the emulator, verify against a real bucket
  // before touching the production code.
  it("refuses to overwrite an existing raw object", async () => {
    mockWebhook();
    // Pin the clock and the randomness so the second call reuses the id.
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-31T01:41:38.123Z"));
    vi.spyOn(crypto, "getRandomValues").mockImplementation((a) => a);

    const first = await ingest(message(), env, createExecutionContext());
    const other = new TextEncoder().encode("Subject: different\r\n\r\nother\r\n");
    await expect(ingest(message(undefined, undefined, other), env, createExecutionContext())).rejects.toThrow(
      "already exists",
    );

    const raw = await env.RAW.get(first.raw_key);
    expect(new Uint8Array(await raw!.arrayBuffer())).toEqual(RAW);
  });

  it.each([
    ["rejects", async () => Promise.reject(new TypeError("network down"))],
    ["answers 500", async () => new Response("boom", { status: 500 })],
  ])("does not throw when the webhook %s", async (_, impl) => {
    const fetch = mockWebhook(impl);
    const ctx = createExecutionContext();
    await expect(worker.email(message(), env, ctx)).resolves.toBeUndefined();
    await expect(waitOnExecutionContext(ctx)).resolves.toBeUndefined();
    expect(fetch).toHaveBeenCalledOnce();
  });
});

// Verifies with crypto.subtle.verify rather than the Worker's sign(), so a
// bug in sign() cannot vouch for itself; sign() itself is pinned by hmac.json.
async function verify(secret: string, timestamp: number, body: string, header: string): Promise<boolean> {
  const hex = /^v1=([0-9a-f]{64})$/.exec(header)?.[1];
  if (!hex) return false;
  const mac = Uint8Array.from(hex.match(/../g)!, (h) => parseInt(h, 16));
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
    "verify",
  ]);
  return crypto.subtle.verify("HMAC", key, mac, enc.encode(`v1:${timestamp}:${body}`));
}
