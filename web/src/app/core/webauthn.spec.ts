import {
  base64UrlToBuffer,
  bufferToBase64Url,
  credentialToJSON,
  toCreationOptions,
  toRequestOptions,
  webAuthnSupported,
} from './webauthn';

// A silent mismatch in this translation does not error — it produces
// ceremonies that never verify. Hence tests on the exact byte behaviour.

describe('base64url codec', () => {
  it('round-trips arbitrary bytes', () => {
    const bytes = new Uint8Array([0, 1, 2, 250, 251, 252, 253, 254, 255]);
    const encoded = bufferToBase64Url(bytes.buffer);
    expect(new Uint8Array(base64UrlToBuffer(encoded))).toEqual(bytes);
  });

  it('emits the URL-safe alphabet without padding', () => {
    // 0xfb 0xef 0xbe encodes to "++++" in plain base64: the URL-safe variant
    // must use '-' and '_' and strip '=' — the server refuses anything else.
    const encoded = bufferToBase64Url(new Uint8Array([0xfb, 0xef, 0xbe]).buffer);
    expect(encoded).not.toMatch(/[+/=]/);
    expect(new Uint8Array(base64UrlToBuffer(encoded))).toEqual(new Uint8Array([0xfb, 0xef, 0xbe]));
  });

  it('accepts unpadded input of every length residue', () => {
    for (const len of [1, 2, 3, 4, 5]) {
      const bytes = new Uint8Array(Array.from({ length: len }, (_, i) => i * 37));
      expect(new Uint8Array(base64UrlToBuffer(bufferToBase64Url(bytes.buffer)))).toEqual(bytes);
    }
  });
});

describe('creation options translation', () => {
  it('decodes the binary fields and keeps the rest verbatim', () => {
    const options = toCreationOptions({
      publicKey: {
        challenge: bufferToBase64Url(new Uint8Array([1, 2, 3]).buffer),
        rp: { id: 'paas.example.com', name: 'AkerDock' },
        user: {
          id: bufferToBase64Url(new Uint8Array([9, 9]).buffer),
          name: 'op@example.com',
          displayName: 'Op',
        },
        excludeCredentials: [
          { id: bufferToBase64Url(new Uint8Array([7]).buffer), type: 'public-key' },
        ],
        attestation: 'none',
      },
    });
    const pk = options.publicKey!;
    expect(new Uint8Array(pk.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(new Uint8Array(pk.user.id as ArrayBuffer)).toEqual(new Uint8Array([9, 9]));
    expect(new Uint8Array(pk.excludeCredentials![0].id as ArrayBuffer)).toEqual(
      new Uint8Array([7]),
    );
    expect(pk.attestation).toBe('none');
    expect(pk.rp.id).toBe('paas.example.com');
  });
});

describe('request options translation', () => {
  it('handles the discoverable flow, where allowCredentials is absent', () => {
    const options = toRequestOptions({
      publicKey: {
        challenge: bufferToBase64Url(new Uint8Array([4, 5]).buffer),
        rpId: 'paas.example.com',
        userVerification: 'required',
      },
    });
    const pk = options.publicKey!;
    expect(new Uint8Array(pk.challenge as ArrayBuffer)).toEqual(new Uint8Array([4, 5]));
    expect(pk.allowCredentials).toBeUndefined();
    expect(pk.userVerification).toBe('required');
  });

  it('decodes an explicit allow-list', () => {
    const options = toRequestOptions({
      publicKey: {
        challenge: bufferToBase64Url(new Uint8Array([4, 5]).buffer),
        allowCredentials: [
          {
            id: bufferToBase64Url(new Uint8Array([8, 9]).buffer),
            type: 'public-key',
            transports: ['usb'],
          },
        ],
      },
    });
    const credential = options.publicKey!.allowCredentials![0];
    expect(new Uint8Array(credential.id as ArrayBuffer)).toEqual(new Uint8Array([8, 9]));
    expect(credential.transports).toEqual(['usb']);
  });
});

describe('credential serialization', () => {
  const bytes = (...values: number[]) => new Uint8Array(values).buffer;

  it('uses the browser native serializer when available', () => {
    const native = { id: 'native' };
    const credential = { toJSON: () => native } as unknown as PublicKeyCredential;
    expect(credentialToJSON(credential)).toBe(native);
  });

  it('serializes an attestation response on older browsers', () => {
    const credential = {
      id: 'registration',
      rawId: bytes(1),
      type: 'public-key',
      authenticatorAttachment: null,
      getClientExtensionResults: () => ({ credProps: true }),
      response: {
        clientDataJSON: bytes(2),
        attestationObject: bytes(3),
        getTransports: () => ['internal'],
      },
    } as unknown as PublicKeyCredential;

    const out = credentialToJSON(credential) as {
      authenticatorAttachment?: string;
      response: { attestationObject: string; transports: string[] };
    };
    expect(out.authenticatorAttachment).toBeUndefined();
    expect(out.response.attestationObject).toBe(bufferToBase64Url(bytes(3)));
    expect(out.response.transports).toEqual(['internal']);
  });

  it('serializes an assertion response and its optional user handle', () => {
    const credential = {
      id: 'login',
      rawId: bytes(1),
      type: 'public-key',
      authenticatorAttachment: 'platform',
      getClientExtensionResults: () => ({}),
      response: {
        clientDataJSON: bytes(2),
        authenticatorData: bytes(3),
        signature: bytes(4),
        userHandle: bytes(5),
      },
    } as unknown as PublicKeyCredential;

    const out = credentialToJSON(credential) as {
      response: { signature: string; userHandle: string | null };
    };
    expect(out.response.signature).toBe(bufferToBase64Url(bytes(4)));
    expect(out.response.userHandle).toBe(bufferToBase64Url(bytes(5)));
  });

  it('reports WebAuthn support in the headless browser', () => {
    expect(webAuthnSupported()).toBeTrue();
  });
});
