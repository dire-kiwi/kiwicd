// Seals secret values in the browser to the secrets service's public key, so
// the server only ever sees them encrypted. This is HPKE (RFC 9180) base mode
// with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and AES-256-GCM, the suite
// internal/sealed opens; see there for the format.

export interface SecretScope {
  repository?: string;
  environment?: string;
  name: string;
}

const enc = new TextEncoder();

function concat(...parts: Uint8Array[]): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let i = 0;
  for (const p of parts) {
    out.set(p, i);
    i += p.length;
  }
  return out;
}

function i2osp(n: number, len: number): Uint8Array {
  const out = new Uint8Array(len);
  for (let i = len - 1; i >= 0; i--, n >>= 8) out[i] = n & 0xff;
  return out;
}

async function hmac(key: Uint8Array, data: Uint8Array): Promise<Uint8Array> {
  // HKDF's empty salt is HashLen zero bytes, which HMAC treats like no key;
  // WebCrypto refuses zero-length HMAC keys.
  const k = await crypto.subtle.importKey("raw", concat(key.length ? key : new Uint8Array(32)), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, concat(data)));
}

const KEM_ID = 0x0020; // DHKEM(X25519, HKDF-SHA256)
const KDF_ID = 0x0001; // HKDF-SHA256
const AEAD_ID = 0x0002; // AES-256-GCM
const v1 = enc.encode("HPKE-v1");

function labeledExtract(suite: Uint8Array, salt: Uint8Array, label: string, ikm: Uint8Array) {
  return hmac(salt, concat(v1, suite, enc.encode(label), ikm));
}

// Every output here is at most one SHA-256 block, so HKDF-Expand is one HMAC.
async function labeledExpand(suite: Uint8Array, prk: Uint8Array, label: string, info: Uint8Array, len: number) {
  const t = await hmac(prk, concat(i2osp(len, 2), v1, suite, enc.encode(label), info, new Uint8Array([1])));
  return t.slice(0, len);
}

function base64(b: Uint8Array): string {
  let s = "";
  for (const c of b) s += String.fromCharCode(c);
  return btoa(s);
}

function unbase64(s: string): Uint8Array {
  return Uint8Array.from(atob(s.trim()), (c) => c.charCodeAt(0));
}

/** The key's ID, as the server and the secrets service name it: the first 8 bytes of its SHA-256, in hex. */
export async function keyID(publicKey: string): Promise<string> {
  const h = new Uint8Array(await crypto.subtle.digest("SHA-256", concat(unbase64(publicKey))));
  return Array.from(h.slice(0, 8), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Encrypts value for scope to publicKey (base64 X25519), returning "pk1:KEYID:BASE64". */
export async function seal(publicKey: string, scope: SecretScope, value: string): Promise<string> {
  const pkR = unbase64(publicKey);
  if (pkR.length !== 32) throw new Error("the secrets public key is not a 32-byte X25519 key");
  const eph = (await crypto.subtle.generateKey({ name: "X25519" }, true, ["deriveBits"])) as CryptoKeyPair;
  const pkE = new Uint8Array(await crypto.subtle.exportKey("raw", eph.publicKey));
  const recipient = await crypto.subtle.importKey("raw", concat(pkR), { name: "X25519" }, false, []);
  const dh = new Uint8Array(await crypto.subtle.deriveBits({ name: "X25519", public: recipient }, eph.privateKey, 256));

  const kemSuite = concat(enc.encode("KEM"), i2osp(KEM_ID, 2));
  const eaePRK = await labeledExtract(kemSuite, new Uint8Array(), "eae_prk", dh);
  const shared = await labeledExpand(kemSuite, eaePRK, "shared_secret", concat(pkE, pkR), 32);

  const suite = concat(enc.encode("HPKE"), i2osp(KEM_ID, 2), i2osp(KDF_ID, 2), i2osp(AEAD_ID, 2));
  const pskIDHash = await labeledExtract(suite, new Uint8Array(), "psk_id_hash", new Uint8Array());
  const infoHash = await labeledExtract(suite, new Uint8Array(), "info_hash", enc.encode("kiwicd secrets v1"));
  const context = concat(new Uint8Array([0]), pskIDHash, infoHash);
  const secret = await labeledExtract(suite, shared, "secret", new Uint8Array());
  const key = await labeledExpand(suite, secret, "key", context, 32);
  const nonce = await labeledExpand(suite, secret, "base_nonce", context, 12);

  const aad = enc.encode(`kiwicd secret\0${scope.repository ?? ""}\0${scope.environment ?? ""}\0${scope.name}`);
  const aesKey = await crypto.subtle.importKey("raw", concat(key), "AES-GCM", false, ["encrypt"]);
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: concat(nonce), additionalData: concat(aad) }, aesKey, enc.encode(value)));
  return `pk1:${await keyID(publicKey)}:${base64(concat(pkE, ct))}`;
}
