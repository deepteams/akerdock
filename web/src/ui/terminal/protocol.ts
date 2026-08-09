/**
 * The terminal wire protocol (§24.4, ADR-024), as pure functions — no DOM, no
 * socket. The component below is thin on purpose: everything worth testing
 * lives here.
 *
 * The contract's `websocket_path` is same-origin (the control plane has one
 * port, §27.1), so the scheme follows the page: https → wss, http → ws. Get
 * that wrong on an HTTPS instance and the browser blocks the connection as
 * mixed content — with a console message and nothing in the UI.
 */

/** The session as the API returns it — the token is shown once (§23.2). */
export interface TerminalSessionInfo {
  uuid: string;
  target_kind: 'server' | 'container';
  target_name: string;
  websocket_path: string;
  token: string;
  token_expires_at: string;
  idle_timeout_seconds: number;
  max_duration_seconds: number;
}

export interface Geometry {
  cols: number;
  rows: number;
}

/**
 * Builds the attach URL. The token rides in the query string because the
 * browser WebSocket API cannot set headers — it is single-use and expires in
 * a minute, which is what makes that acceptable (§24.4).
 */
export function attachUrl(
  session: TerminalSessionInfo,
  origin: string,
  geometry: Geometry,
): string {
  const url = new URL(session.websocket_path, origin);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('token', session.token);
  url.searchParams.set('cols', String(geometry.cols));
  url.searchParams.set('rows', String(geometry.rows));
  return url.toString();
}

/** A resize control frame — the only message the client sends as text. */
export function resizeMessage(geometry: Geometry): string {
  return JSON.stringify({ type: 'resize', cols: geometry.cols, rows: geometry.rows });
}

/**
 * Why the server says the session ended.
 *
 * The last three arrived with ADR-066 and ADR-067 and are the reasons a shell
 * stops for something that is not the developer, the clock or an administrator:
 * the target was never reached, it vanished under the session, or it was asleep
 * and did not wake. They are exactly the cases where a browser tab that simply
 * goes quiet reads as a bug in the platform.
 */
export type EndReason =
  | 'user_close'
  | 'idle_timeout'
  | 'max_duration'
  | 'disconnect'
  | 'revoked'
  | 'target_unreachable'
  | 'target_stopped'
  | 'wake_failed';

/**
 * Reads the server's end frame. Anything else (terminal output arrives as
 * binary) yields null — a text frame we do not understand is not an end.
 */
export function parseEndMessage(data: string): EndReason | null {
  try {
    const msg = JSON.parse(data) as { type?: string; reason?: string };
    if (msg.type !== 'end') return null;
    return (msg.reason as EndReason) ?? 'disconnect';
  } catch {
    return null;
  }
}

/** What the operator reads when a session ends — never a bare enum value. */
export function describeEnd(reason: EndReason): string {
  switch (reason) {
    case 'user_close':
      return 'Session closed.';
    case 'idle_timeout':
      return 'Session closed after inactivity (idle timeout).';
    case 'max_duration':
      return 'Session closed: maximum duration reached.';
    case 'revoked':
      return 'Session revoked by the server.';
    case 'disconnect':
      return 'Connection lost.';
    case 'target_unreachable':
      return 'Session ended: the target could not be reached — check the server, its agent and the container.';
    case 'target_stopped':
      return 'Session ended: the container stopped — a redeploy, a manual stop, or a scale-to-zero sleep.';
    case 'wake_failed':
      return 'Session ended: the target was asleep and could not be woken. Try again.';
  }
}
