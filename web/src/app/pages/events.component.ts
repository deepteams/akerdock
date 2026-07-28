import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, OnDestroy, inject, signal } from '@angular/core';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import {
  APPLICATION_EVENTS,
  DEPLOYMENT_EVENTS,
  LiveEventsService,
  PREVIEW_EVENTS,
} from '../core/live-refresh';

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
  imports: [SlicePipe, CardComponent, EmptyStateComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div class="head">
          <h1>Events</h1>
          <span class="akd-badge" [class.akd-badge--ok]="connected()" role="status">
            {{ connected() ? 'live' : 'connecting…' }}
          </span>
        </div>
      </header>

      @if (events().length === 0) {
        <akd-empty-state
          icon="activity"
          title="Waiting for events."
          message="Deployments, job transitions and alerts appear here as they happen."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Live instance events, newest first
            </caption>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Event</th>
                <th scope="col">Severity</th>
                <th scope="col">Payload</th>
              </tr>
            </thead>
            <tbody>
              @for (event of events(); track event.id) {
                @let sev = severity(event.type);
                <tr>
                  <td class="akd-mono time">{{ event.at | slice: 11 : 19 }}</td>
                  <td class="akd-mono type">{{ event.type }}</td>
                  <td>
                    <span
                      class="akd-badge"
                      [class.akd-badge--danger]="sev === 'error'"
                      [class.akd-badge--warn]="sev === 'warning'"
                    >
                      {{ sev }}
                    </span>
                  </td>
                  <td class="akd-mono payload">{{ event.payload }}</td>
                </tr>
              }
            </tbody>
          </table>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
      }
      .time {
        color: var(--text-3);
        white-space: nowrap;
        vertical-align: top;
      }
      .type {
        white-space: nowrap;
        vertical-align: top;
      }
      .payload {
        color: var(--text-3);
        overflow-wrap: anywhere;
      }
    `,
  ],
})
export class EventsComponent implements OnDestroy {
  private readonly live = inject(LiveEventsService);
  private readonly stopLive: () => void;

  protected readonly events = signal<LiveEvent[]>([]);
  protected readonly connected = this.live.connected;

  /**
   * The server names its SSE events (`event: deployment.failed.v1`), and
   * EventSource fires NAMED events only for registered listeners — there is no
   * wildcard. So the feed subscribes to the catalogue explicitly: the static
   * outbox types plus the shared lifecycle catalogues. An event type absent
   * from this list is invisible here (and only here) — when a new type is
   * added server-side, add it below (or to the shared catalogues).
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
    ...APPLICATION_EVENTS,
    ...PREVIEW_EVENTS,
    ...DEPLOYMENT_EVENTS,
  ];

  constructor() {
    // One shared stream for the whole app (LiveEventsService): this page is a
    // subscriber like any other, never a second connection.
    this.stopLive = this.live.subscribe(EventsComponent.eventTypes, (type, msg) =>
      this.push(type, msg),
    );
  }

  /**
   * Severity is read from the event name itself — the raw feed carries no
   * other structured field. Word boundaries keep `drill_failed` from matching
   * `failed` (underscore is a word character), hence its own alternative.
   */
  protected severity(type: string): 'error' | 'warning' | 'info' {
    if (/\b(failed|dead_letter|drill_failed|unreachable)\b/.test(type)) return 'error';
    if (/\b(expiring|partial|cancelled)\b/.test(type)) return 'warning';
    return 'info';
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
    this.stopLive();
  }
}
