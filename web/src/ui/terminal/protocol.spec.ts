import { attachUrl, describeEnd, parseEndMessage, resizeMessage } from './protocol';
import type { TerminalSessionInfo } from './protocol';

function session(overrides: Partial<TerminalSessionInfo> = {}): TerminalSessionInfo {
  return {
    uuid: 'e2b1…',
    target_kind: 'container',
    target_name: 'api',
    websocket_path: '/terminal/ws',
    token: 'akdt_secret',
    token_expires_at: '2026-07-14T10:00:00Z',
    idle_timeout_seconds: 900,
    max_duration_seconds: 14400,
    ...overrides,
  };
}

describe('terminal protocol', () => {
  // Getting the scheme wrong on an HTTPS instance is not a cosmetic bug: the
  // browser blocks a ws:// connection from an https:// page as mixed content,
  // and the only trace is a console line.
  it('upgrades the page scheme: https → wss', () => {
    const url = new URL(attachUrl(session(), 'https://akerdock.example.com', { cols: 80, rows: 24 }));
    expect(url.protocol).toBe('wss:');
    expect(url.host).toBe('akerdock.example.com');
    expect(url.pathname).toBe('/terminal/ws');
  });

  it('uses ws on a plain-http instance', () => {
    const url = new URL(attachUrl(session(), 'http://localhost:8080', { cols: 80, rows: 24 }));
    expect(url.protocol).toBe('ws:');
    expect(url.host).toBe('localhost:8080');
  });

  it('carries the one-time token and the initial geometry', () => {
    const url = new URL(attachUrl(session(), 'https://x.example', { cols: 132, rows: 43 }));
    expect(url.searchParams.get('token')).toBe('akdt_secret');
    expect(url.searchParams.get('cols')).toBe('132');
    expect(url.searchParams.get('rows')).toBe('43');
  });

  it('encodes a resize control frame', () => {
    expect(JSON.parse(resizeMessage({ cols: 100, rows: 30 }))).toEqual({
      type: 'resize',
      cols: 100,
      rows: 30,
    });
  });

  it('reads the end frame', () => {
    expect(parseEndMessage('{"type":"end","reason":"idle_timeout"}')).toBe('idle_timeout');
  });

  // Terminal output arrives as binary; any text frame that is not an end
  // message must not be mistaken for one — least of all a shell printing JSON.
  it('is not fooled by other text', () => {
    expect(parseEndMessage('{"type":"other"}')).toBeNull();
    expect(parseEndMessage('total 0')).toBeNull();
    expect(parseEndMessage('')).toBeNull();
  });

  it('explains every end reason in words', () => {
    for (const reason of ['user_close', 'idle_timeout', 'max_duration', 'disconnect', 'revoked'] as const) {
      expect(describeEnd(reason).length).toBeGreaterThan(0);
    }
  });
});
