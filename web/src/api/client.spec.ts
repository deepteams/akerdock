import { AkerDockClient, ApiError } from './client';

// The dashboard hands the client a RELATIVE baseUrl (/api/v1) and lets the
// page origin disambiguate. That is exactly what a bare `new URL()` cannot
// parse — the regression here rendered the whole dashboard down with
// "Failed to construct 'URL': Invalid URL" on the first request.
describe('AkerDockClient', () => {
  function capturingFetch(status = 200, body = '{}') {
    const calls: URL[] = [];
    const fetchImpl = (async (input: URL | RequestInfo) => {
      calls.push(input as URL);
      return new Response(body, {
        status,
        headers: { 'Content-Type': 'application/json' },
      });
    }) as typeof globalThis.fetch;
    return { calls, fetchImpl };
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
});
