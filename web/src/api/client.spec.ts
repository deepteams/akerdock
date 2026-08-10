import { AkerDockClient, ApiError } from './client';

// The dashboard hands the client a RELATIVE baseUrl (/api/v1) and lets the
// page origin disambiguate. That is exactly what a bare `new URL()` cannot
// parse — the regression here rendered the whole dashboard down with
// "Failed to construct 'URL': Invalid URL" on the first request.
describe('AkerDockClient', () => {
  function capturingFetch(status = 200, body = '{}') {
    const calls: URL[] = [];
    const init: RequestInit[] = [];
    const fetchImpl = (async (input: URL | RequestInfo, options?: RequestInit) => {
      calls.push(input as URL);
      init.push(options ?? {});
      return new Response(body, {
        status,
        headers: { 'Content-Type': 'application/json' },
      });
    }) as typeof globalThis.fetch;
    return { calls, init, fetchImpl };
  }

  it('resolves a relative baseUrl against the page origin', async () => {
    const { calls, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });

    await client.request('GET', '/servers');

    expect(calls.length).toBe(1);
    expect(calls[0].href).toBe(`${globalThis.location.origin}/api/v1/servers`);
  });

  it('leaves an absolute baseUrl untouched', async () => {
    const { calls, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({
      baseUrl: 'https://akerdock.example.com/api/v1/',
      fetch: fetchImpl,
    });

    await client.request('GET', '/servers', { query: { page: 2 } });

    expect(calls[0].href).toBe('https://akerdock.example.com/api/v1/servers?page=2');
  });

  it('surfaces a structured error body as ApiError', async () => {
    const { fetchImpl } = capturingFetch(
      409,
      JSON.stringify({ code: 'version_conflict', message: 'stale', details: [] }),
    );
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });

    await expectAsync(client.request('PATCH', '/servers/x', { ifMatch: 3 })).toBeRejectedWithError(
      ApiError,
      'stale',
    );
  });

  it('applies authentication, CSRF, concurrency and idempotency headers', async () => {
    const { init, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({
      baseUrl: '/api/v1/',
      token: 'bearer-token',
      csrfToken: 'csrf-token',
      fetch: fetchImpl,
    });

    await client.request('POST', '/applications', {
      ifMatch: 7,
      idempotencyKey: 'stable-request',
      body: { name: 'unit' },
      query: { enabled: true, ignored: undefined },
    });

    const headers = init[0].headers as Record<string, string>;
    expect(headers['Authorization']).toBe('Bearer bearer-token');
    expect(headers['X-CSRF-Token']).toBe('csrf-token');
    expect(headers['If-Match']).toBe('"7"');
    expect(headers['Idempotency-Key']).toBe('stable-request');
    expect(headers['Content-Type']).toBe('application/json');
    expect(init[0].credentials).toBe('same-origin');
    expect(init[0].body).toBe('{"name":"unit"}');
  });

  it('returns undefined for an empty 204 response', async () => {
    const fetchImpl = (async () => new Response(null, { status: 204 })) as typeof globalThis.fetch;
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });

    expect(await client.request<void>('DELETE', '/applications/unit')).toBeUndefined();
  });

  it('uses safe fallback fields for an empty error response', async () => {
    const fetchImpl = (async () =>
      new Response('', { status: 500, statusText: 'Server Error' })) as typeof globalThis.fetch;
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });

    try {
      await client.request('GET', '/broken');
      fail('request should reject');
    } catch (error) {
      const apiError = error as ApiError;
      expect(apiError.code).toBe('unknown');
      expect(apiError.message).toBe('Server Error');
      expect(apiError.isVersionConflict).toBeFalse();
      expect(apiError.hasRemnants).toBeFalse();
    }
  });

  it('classifies the two actionable conflict types', () => {
    expect(new ApiError(409, 'version_conflict', 'stale').isVersionConflict).toBeTrue();
    expect(new ApiError(409, 'remnants_present', 'cleanup').hasRemnants).toBeTrue();
  });

  it('keeps every typed shortcut connected to the shared request policy', async () => {
    const { calls, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });
    const prototype = AkerDockClient.prototype as unknown as Record<
      string,
      (...args: unknown[]) => unknown
    >;
    const shortcutNames = Object.getOwnPropertyNames(AkerDockClient.prototype).filter(
      (name) =>
        !['constructor', 'request', 'deploymentLogs', 'streamApplicationLogs', 'events'].includes(
          name,
        ),
    );

    for (const name of shortcutNames) {
      const before = calls.length;
      const value = prototype[name].apply(client, ['unit', 'unit', 1, {}, {}, {}]);
      await value;
      expect(calls.length)
        .withContext(`${name} must delegate exactly once to request()`)
        .toBe(before + 1);
    }

    expect(shortcutNames.length).toBeGreaterThan(150);
  });

  // The manual is filtered SERVER-side (ADR-072 §4): asking for the whole of
  // it is a different request, not a different filter, so the parameter has to
  // reach the wire — and has to be absent when nobody asked.
  it('asks for the whole manual only when told to', async () => {
    const { calls, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });

    await client.getManual();
    expect(calls[0].pathname).toBe('/api/v1/docs');
    expect(calls[0].search).toBe('');

    await client.getManual({ all: true });
    expect(calls[1].searchParams.get('all')).toBe('true');

    await client.getManual({ all: false });
    expect(calls[2].search).toBe('');
  });

  it('covers shortcut defaults and the native fetch fallback', async () => {
    // Construction without a fetch override is important for the real
    // dashboard; no request is made with this instance.
    expect(new AkerDockClient({ baseUrl: '/api/v1' })).toBeTruthy();

    const { calls, fetchImpl } = capturingFetch();
    const client = new AkerDockClient({ baseUrl: '/api/v1', fetch: fetchImpl });
    await client.deployApplication('unit');
    expect(calls[0].pathname).toBe('/api/v1/applications/unit/deploy');
  });

  it('builds resumable EventSource URLs without opening a real stream', () => {
    const original = globalThis.EventSource;
    const seen: { url: string; options?: EventSourceInit }[] = [];
    class FakeEventSource {
      constructor(url: string | URL, options?: EventSourceInit) {
        seen.push({ url: String(url), options });
      }
    }
    Object.defineProperty(globalThis, 'EventSource', {
      configurable: true,
      value: FakeEventSource,
    });
    try {
      const client = new AkerDockClient({
        baseUrl: '/api/v1',
        token: 'stream-token',
        fetch: capturingFetch().fetchImpl,
      });
      client.deploymentLogs('deployment-1');
      client.events({ lastEventId: '42' });
      client.streamApplicationLogs('app-1', { component: 'db' });

      expect(new URL(seen[0].url).searchParams.get('access_token')).toBe('stream-token');
      expect(new URL(seen[1].url).searchParams.get('last_event_id')).toBe('42');
      expect(seen[0].options?.withCredentials).toBeTrue();
      expect(new URL(seen[2].url).pathname).toBe('/api/v1/applications/app-1/logs/stream');
      expect(new URL(seen[2].url).searchParams.get('component')).toBe('db');
      expect(new URL(seen[2].url).searchParams.get('access_token')).toBe('stream-token');

      const cookieClient = new AkerDockClient({
        baseUrl: '/api/v1',
        fetch: capturingFetch().fetchImpl,
      });
      cookieClient.events();
      cookieClient.streamApplicationLogs('app-2');
      expect(new URL(seen[3].url).search).toBe('');
      expect(new URL(seen[4].url).search).toBe('');
    } finally {
      Object.defineProperty(globalThis, 'EventSource', {
        configurable: true,
        value: original,
      });
    }
  });
});
