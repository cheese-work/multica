import type { PasskeyAssertionEnvelope, PasskeyChallenge } from "../api/passkey-schema";

const ALPHABET = /^[A-Za-z0-9_-]*$/;

export function base64urlToBytes(text: string): Uint8Array {
  if (!ALPHABET.test(text)) throw new Error("invalid base64url");
  const padded = text.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(text.length / 4) * 4, "=");
  return Uint8Array.from(atob(padded), (char) => char.charCodeAt(0));
}

export function bytesToBase64url(data: ArrayBuffer | Uint8Array): string {
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

type Descriptor = { id: string; type: string; transports?: string[] };
type Options = { publicKey: Record<string, unknown> };

function decodeOptions(options: Options): Record<string, unknown> {
  const key = options.publicKey as {
    challenge: string; user?: { id: string }; excludeCredentials?: Descriptor[]; allowCredentials?: Descriptor[];
  };
  const decodeAll = (list?: Descriptor[]) => list?.map((item) => ({ ...item, id: base64urlToBytes(item.id) }));
  return {
    ...key,
    challenge: base64urlToBytes(key.challenge),
    ...(key.user && { user: { ...key.user, id: base64urlToBytes(key.user.id) } }),
    ...(key.excludeCredentials && { excludeCredentials: decodeAll(key.excludeCredentials) }),
    ...(key.allowCredentials && { allowCredentials: decodeAll(key.allowCredentials) }),
  };
}

function credentials(): CredentialsContainer {
  const container = typeof navigator === "undefined" ? undefined : navigator.credentials;
  if (!container) throw new Error("passkeys are not supported in this browser");
  return container;
}

function envelope(credential: PublicKeyCredential, response: Record<string, unknown>) {
  return {
    id: credential.id,
    rawId: bytesToBase64url(credential.rawId),
    type: credential.type,
    ...(credential.authenticatorAttachment && { authenticatorAttachment: credential.authenticatorAttachment }),
    response,
    clientExtensionResults: credential.getClientExtensionResults(),
  };
}

/** Runs `navigator.credentials.create` on server options; returns the attestation JSON the server parses. */
export async function createPasskey(options: Options) {
  const credential = (await credentials().create({
    publicKey: decodeOptions(options) as unknown as PublicKeyCredentialCreationOptions,
  })) as PublicKeyCredential | null;
  if (!credential) throw new Error("passkey creation was cancelled");
  const response = credential.response as AuthenticatorAttestationResponse;
  return envelope(credential, {
    attestationObject: bytesToBase64url(response.attestationObject),
    clientDataJSON: bytesToBase64url(response.clientDataJSON),
    transports: response.getTransports?.() ?? [],
  });
}

/** Runs `navigator.credentials.get` on server options; returns the assertion JSON the server parses. */
export async function getPasskeyAssertion(options: Options) {
  const credential = (await credentials().get({
    publicKey: decodeOptions(options) as unknown as PublicKeyCredentialRequestOptions,
  })) as PublicKeyCredential | null;
  if (!credential) throw new Error("passkey sign-in was cancelled");
  const response = credential.response as AuthenticatorAssertionResponse;
  return envelope(credential, {
    authenticatorData: bytesToBase64url(response.authenticatorData),
    clientDataJSON: bytesToBase64url(response.clientDataJSON),
    signature: bytesToBase64url(response.signature),
    ...(response.userHandle && { userHandle: bytesToBase64url(response.userHandle) }),
  });
}

/** Signs a server challenge and wraps the credential in the envelope that revoke and approve post. */
export async function signPasskeyChallenge(challenge: PasskeyChallenge): Promise<PasskeyAssertionEnvelope> {
  return { challenge_id: challenge.challenge_id, credential: await getPasskeyAssertion(challenge.options) };
}
