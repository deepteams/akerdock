/**
 * WebAuthn wire helpers.
 *
 * The server (go-webauthn) speaks the W3C JSON serialization: every binary
 * field travels as base64url WITHOUT padding. The browser API speaks
 * ArrayBuffers. These functions are the only place that translation happens —
 * deterministic, dependency-free, and unit-tested, because a silent mismatch
 * here does not error: it produces ceremonies that never verify.
 */

export function base64UrlToBuffer(value: string): ArrayBuffer {
  const base64 = value.replace(/-/g, '+').replace(/_/g, '/');
  const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4);
  const raw = atob(padded);
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes.buffer;
}

export function bufferToBase64Url(value: ArrayBuffer): string {
  const bytes = new Uint8Array(value);
  let raw = '';
  for (const b of bytes) raw += String.fromCharCode(b);
  return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** The server's registration options, as JSON. Only the fields we translate
 *  are typed; the rest passes through untouched. */
interface CreationOptionsJSON {
  publicKey: {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: { id: string; type: string; transports?: string[] }[];
    [key: string]: unknown;
  };
}

interface RequestOptionsJSON {
  publicKey: {
    challenge: string;
    allowCredentials?: { id: string; type: string; transports?: string[] }[];
    [key: string]: unknown;
  };
}

/** Server JSON → what navigator.credentials.create() expects. */
export function toCreationOptions(json: CreationOptionsJSON): CredentialCreationOptions {
  const pk = json.publicKey;
  return {
    publicKey: {
      ...pk,
      challenge: base64UrlToBuffer(pk.challenge),
      user: { ...pk.user, id: base64UrlToBuffer(pk.user.id) },
      excludeCredentials: pk.excludeCredentials?.map((c) => ({
        ...c,
        type: 'public-key' as const,
        id: base64UrlToBuffer(c.id),
        transports: c.transports as AuthenticatorTransport[] | undefined,
      })),
    } as PublicKeyCredentialCreationOptions,
  };
}

/** Server JSON → what navigator.credentials.get() expects. */
export function toRequestOptions(json: RequestOptionsJSON): CredentialRequestOptions {
  const pk = json.publicKey;
  return {
    publicKey: {
      ...pk,
      challenge: base64UrlToBuffer(pk.challenge),
      allowCredentials: pk.allowCredentials?.map((c) => ({
        ...c,
        type: 'public-key' as const,
        id: base64UrlToBuffer(c.id),
        transports: c.transports as AuthenticatorTransport[] | undefined,
      })),
    } as PublicKeyCredentialRequestOptions,
  };
}

/**
 * Browser credential → the W3C JSON the server parses.
 *
 * The native toJSON() is used when present (it IS this serialization); the
 * manual path covers older engines, byte for byte identical.
 */
export function credentialToJSON(credential: PublicKeyCredential): unknown {
  const maybeNative = credential as PublicKeyCredential & { toJSON?: () => unknown };
  if (typeof maybeNative.toJSON === 'function') return maybeNative.toJSON();

  const base = {
    id: credential.id,
    rawId: bufferToBase64Url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults(),
  };

  const response = credential.response;
  if (isAttestation(response)) {
    return {
      ...base,
      response: {
        clientDataJSON: bufferToBase64Url(response.clientDataJSON),
        attestationObject: bufferToBase64Url(response.attestationObject),
        transports: response.getTransports?.() ?? [],
      },
    };
  }
  const assertion = response as AuthenticatorAssertionResponse;
  return {
    ...base,
    response: {
      clientDataJSON: bufferToBase64Url(assertion.clientDataJSON),
      authenticatorData: bufferToBase64Url(assertion.authenticatorData),
      signature: bufferToBase64Url(assertion.signature),
      userHandle: assertion.userHandle ? bufferToBase64Url(assertion.userHandle) : null,
    },
  };
}

function isAttestation(r: AuthenticatorResponse): r is AuthenticatorAttestationResponse {
  return 'attestationObject' in r;
}

/** Passkeys need a platform that has the API at all. */
export function webAuthnSupported(): boolean {
  return typeof globalThis.PublicKeyCredential !== 'undefined';
}
