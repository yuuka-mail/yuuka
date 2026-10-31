import { describe, expect, it } from "vitest";
import hmac from "../../testdata/contract/v1/hmac.json";
import keys from "../../testdata/contract/v1/keys.json";
import markers from "../../testdata/contract/v1/markers.json";
import metadata from "../../testdata/contract/v1/metadata.json";
import { type Marker, base64url, pendingKey, rawKey, serializeMarker, sign } from "../src/contract";
import { ulid } from "../src/ulid";
import { decodeBase64url } from "./helpers";

const encoder = new TextEncoder();

describe("markers.json", () => {
  // The Worker only writes markers; parsing (and so the invalid cases) is
  // Go's job. A case whose json carries fields outside v1 pins down that Go
  // ignores them, not something the Worker can produce.
  const producible = markers.valid.filter(
    (c) => Object.keys(JSON.parse(c.json)).length === Object.keys(c.marker).length,
  );

  it.each(producible.map((c) => [c.name, c] as const))("%s serializes byte for byte", (_, c) => {
    // Reverse the key order to prove the output order comes from the contract.
    const shuffled = Object.fromEntries(Object.entries(c.marker).reverse()) as unknown as Marker;
    expect(serializeMarker(shuffled)).toBe(c.json);
  });
});

describe("keys.json", () => {
  it.each(keys.map((c) => [c.name, c] as const))("%s", (_, c) => {
    expect(rawKey(c.id, new Date(c.received_at))).toBe(c.raw_key);
    expect(pendingKey(c.id)).toBe(c.pending_key);
  });
});

describe("metadata.json", () => {
  it.each(metadata.map((c) => [c.name, c] as const))("%s", (_, c) => {
    expect(base64url(encoder.encode(c.marker_json))).toBe(c.yuuka_marker);
    expect(decodeBase64url(c.yuuka_marker)).toBe(c.marker_json);
  });
});

describe("hmac.json", () => {
  it.each(hmac.sign.map((c) => [c.name, c] as const))("signs %s", async (_, c) => {
    expect(await sign(c.secret, c.timestamp, encoder.encode(c.body))).toBe(c.signature);
  });
});

describe("ulid", () => {
  it("is an uppercase Crockford ULID", () => {
    expect(ulid(Date.now())).toMatch(/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/);
  });

  // The vector ids were minted at their received_at, so their time prefix
  // doubles as an encoding check.
  it.each(keys.map((c) => [c.name, c] as const))("encodes the time of %s", (_, c) => {
    expect(ulid(Date.parse(c.received_at)).slice(0, 10)).toBe(c.id.slice(0, 10));
  });

  it("differs within the same millisecond", () => {
    const now = Date.now();
    expect(ulid(now)).not.toBe(ulid(now));
  });
});
