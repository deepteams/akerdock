import { Injectable, inject, signal } from '@angular/core';
import { ApiService } from './api.service';

/**
 * The ONE live event stream of the app (ADR-024/040). EventSource has no
 * wildcard, so consumers subscribe to explicit type catalogues — but they all
 * share a single connection: every page opening its own EventSource both
 * doubles the server fan-out and starves the browser's per-host connection
 * budget (~6 on HTTP/1) as soon as a few tabs are open.
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

type Handler = (type: string, msg: MessageEvent<string>) => void;

@Injectable({ providedIn: 'root' })
export class LiveEventsService {
  private readonly api = inject(ApiService);
  private source: EventSource | null = null;
  private readonly handlers = new Map<string, Set<Handler>>();
  private subscribers = 0;

  /** Whether the shared stream is currently open (the events page indicator). */
  readonly connected = signal(false);

  /**
   * Subscribes handler to the named event types on the shared stream, opening
   * it on the first subscriber and closing it after the last. Returns the
   * teardown — hand it to ngOnDestroy or an effect's onCleanup.
   */
  subscribe(types: readonly string[], handler: Handler): () => void {
    this.subscribers++;
    for (const type of types) {
      let set = this.handlers.get(type);
      if (!set) {
        set = new Set();
        this.handlers.set(type, set);
        this.listen(type);
      }
      set.add(handler);
    }
    this.ensureOpen();
    return () => {
      this.subscribers--;
      for (const type of types) {
        this.handlers.get(type)?.delete(handler);
      }
      if (this.subscribers <= 0) {
        this.close();
      }
    };
  }

  /**
   * The common page pattern: reload (collapsed over `debounceMs`) on every
   * event `matches` accepts — a deployment emits one event per state, and
   * reloading fourteen times per deploy would hammer the API for nothing.
   */
  refresh(
    types: readonly string[],
    matches: (ev: LiveEventRef) => boolean,
    reload: () => void,
    debounceMs = 400,
  ): () => void {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const stop = this.subscribe(types, (_type, msg) => {
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
    });
    return () => {
      stop();
      if (timer) clearTimeout(timer);
    };
  }

  private ensureOpen(): void {
    if (this.source) return;
    this.source = this.api.client().events();
    this.source.onopen = () => this.connected.set(true);
    this.source.onerror = () => this.connected.set(false);
    for (const type of this.handlers.keys()) {
      this.listen(type);
    }
  }

  private listen(type: string): void {
    this.source?.addEventListener(type, (msg) => {
      for (const handler of this.handlers.get(type) ?? []) {
        handler(type, msg as MessageEvent<string>);
      }
    });
  }

  private close(): void {
    this.subscribers = 0;
    this.source?.close();
    this.source = null;
    this.connected.set(false);
  }
}
