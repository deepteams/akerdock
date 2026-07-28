import type { ApiService } from './api.service';

/**
 * Live refresh over the team SSE stream (ADR-024/040). EventSource fires
 * NAMED events only for registered listeners — no wildcard — so each caller
 * subscribes to an explicit catalogue and reloads (debounced) when an event
 * matches its resource.
 */

/** Every state a deployment walks through — one event per transition. */
export const DEPLOYMENT_EVENTS: readonly string[] = [
  'queued',
  'preparing',
  'cloning',
  'building',
  'pushing',
  'starting',
  'healthchecking',
  'switching',
  'finishing',
  'succeeded',
  'failed',
  'cancelled',
  'retrying',
  'superseded',
].map((status) => `deployment.${status}.v1`);

/** Preview lifecycle, including the scheduler/agent sleep-wake transitions. */
export const PREVIEW_EVENTS: readonly string[] = [
  'application.preview.created.v1',
  'application.preview.updated.v1',
  'application.preview.deleted.v1',
  'application.preview.expiring.v1',
  'application.preview.slept.v1',
  'application.preview.woken.v1',
];

/** Application-level sleep/wake (ADR-037/040). */
export const APPLICATION_EVENTS: readonly string[] = [
  'application.slept.v1',
  'application.woken.v1',
];

/** The resource_uuid field every outbox event carries. */
export interface LiveEventRef {
  resource_uuid?: string;
}

/**
 * Subscribes to the given event types and calls `reload` (collapsed over
 * `debounceMs`) for every event `matches` accepts. Returns the teardown —
 * hand it to an effect's onCleanup or DestroyRef.
 */
export function liveRefresh(
  api: ApiService,
  types: readonly string[],
  matches: (ev: LiveEventRef) => boolean,
  reload: () => void,
  debounceMs = 400,
): () => void {
  const source = api.client().events();
  let timer: ReturnType<typeof setTimeout> | null = null;
  const onEvent = (msg: MessageEvent<string>) => {
    try {
      const ev = JSON.parse(msg.data) as LiveEventRef;
      if (!matches(ev) || timer) return;
      timer = setTimeout(() => {
        timer = null;
        reload();
      }, debounceMs);
    } catch {
      // Malformed frame: never break the page for a bad event.
    }
  };
  for (const type of types) {
    source.addEventListener(type, onEvent);
  }
  return () => {
    source.close();
    if (timer) clearTimeout(timer);
  };
}
