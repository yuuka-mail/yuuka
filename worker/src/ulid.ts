// Crockford base32, uppercase: the contract requires uppercase ULIDs.
const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

// ulid encodes a 48-bit millisecond timestamp (10 chars) followed by 80
// random bits (16 chars). No monotonic counter: ids only need to be unique
// per invocation, and two calls in the same millisecond differ by randomness.
export function ulid(time: number): string {
  let out = "";
  for (let i = 0, t = time; i < 10; i++, t = Math.floor(t / 32)) {
    out = ALPHABET[t % 32] + out;
  }
  let acc = 0;
  let bits = 0;
  for (const b of crypto.getRandomValues(new Uint8Array(10))) {
    acc = (acc << 8) | b;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += ALPHABET[(acc >> bits) & 31];
    }
    acc &= (1 << bits) - 1;
  }
  return out;
}
