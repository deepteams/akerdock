import { ChangeDetectionStrategy, Component, OnDestroy, inject, signal } from '@angular/core';
import { ApiService } from '../core/api.service';

interface LiveEvent {
  id: string;
  type: string;
  at: string;
  payload: string;
}

/**
 * Live event feed (ADR-024): the SSE stream the rest of the dashboard also
 * observes, shown raw. Reconnection is the browser's EventSource doing its
 * job; resumption is `Last-Event-ID`, so a dropped connection replays nothing
 * and skips nothing.
 */
@Component({
  selector: 'app-events',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Events</h1>
        <span class="akd-muted" role="status">
          {{ connected() ? 'live' : 'connecting…' }}
        </span>
      </header>

      @if (events().length === 0) {
        <div class="akd-empty">
          <p><strong>Waiting for events.</strong></p>
          <p>Deployments, job transitions and alerts appear here as they happen.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Live instance events, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Type</th>
              <th scope="col">Payload</th>
            </tr>
          </thead>
          <tbody>
            @for (event of events(); track event.id) {
              <tr>
                <td class="akd-mono type">{{ event.type }}</td>
                <td class="akd-mono payload">{{ event.payload }}</td>
              </tr>
            }
          </tbody>
        </table>
      }
    </div>
  `,
  styles: [
    `
      .type {
        white-space: nowrap;
        vertical-align: top;
      }
      .payload {
        overflow-wrap: anywhere;
      }
    `,
  ],
})
export class EventsComponent implements OnDestroy {
  private readonly api = inject(ApiService);
  private source: EventSource | null = null;

  protected readonly events = signal<LiveEvent[]>([]);
  protected readonly connected = signal(false);

  /**
   * The server names its SSE events (`event: deployment.failed.v1`), and
   * EventSource fires NAMED events only for registered listeners — there is no
   * wildcard. So the feed subscribes to the catalogue explicitly: the static
   * outbox types, plus `deployment.<status>.v1` for every deployment state.
   * An event type absent from this list is invisible here (and only here) —
   * when a new type is added server-side, add it below.
   */
  private static readonly eventTypes = [
    'application.created.v1',
    'backup.drill_failed.v1',
    'backup.failed.v1',
    'backup.partial.v1',
    'certificate.expiring.v1',
    'job.dead_letter.v1',
    'notification.digest.v1',
    'notification.test.v1',
    'scheduled_task.failed.v1',
    'scheduled_task.succeeded.v1',
    'server.unreachable.v1',
    'team.invitation.v1',
    ...[
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
    ].map((status) => `deployment.${status}.v1`),
  ];

  constructor() {
    this.source = this.api.client().events();
    this.source.onopen = () => this.connected.set(true);
    this.source.onerror = () => this.connected.set(false);
    for (const type of EventsComponent.eventTypes) {
      this.source.addEventListener(type, (msg) => this.push(type, msg));
    }
  }

  private push(type: string, msg: MessageEvent<string>): void {
    const entry: LiveEvent = {
      id: msg.lastEventId || crypto.randomUUID(),
      type,
      at: new Date().toISOString(),
      payload: msg.data,
    };
    // Newest first, bounded: an SSE page left open overnight must not hold
    // the night in memory.
    this.events.update((list) => [entry, ...list].slice(0, 200));
  }

  ngOnDestroy(): void {
    this.source?.close();
    this.source = null;
  }
}
