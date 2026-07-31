import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { fetchAll } from '../../core/pagination';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import type { components } from '../../../api/schema';

type PortForwardSession = components['schemas']['PortForwardSessionInfo'];

/**
 * The tunnels currently held by the team — every target kind, not just the
 * declared endpoints: the operational question is what is forwarded out of this
 * team right now, whoever opened it and onto whatever.
 *
 * Cutting one names a reason on the wire, which the CLI prints: a tunnel that
 * dies in silence is read as a bug in the platform, and the developer's next
 * move is to look for a way around it rather than back into it.
 */
@Component({
  selector: 'app-tunnel-sessions-tab',
  standalone: true,
  imports: [FormsModule, DatePipe, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="bar">
      <label class="toggle">
        <input type="checkbox" [(ngModel)]="includeClosed" (ngModelChange)="reload()" />
        Include closed sessions
      </label>
      <button class="akd-btn akd-btn--ghost akd-btn--sm" type="button" (click)="reload()">
        <akd-icon name="refresh-cw" [size]="14" />
        Refresh
      </button>
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (sessions().length === 0) {
      <akd-empty-state
        icon="cable"
        title="No tunnel open right now"
        [message]="
          includeClosed
            ? 'Nothing has been forwarded recently.'
            : 'Nobody on the team is forwarding a port at the moment.'
        "
      />
    } @else {
      <akd-card [title]="includeClosed ? 'Tunnel sessions' : 'Open tunnels'">
        <table class="akd-table">
          <thead>
            <tr>
              <th>Target</th>
              <th>Who</th>
              <th>From</th>
              <th>Opened</th>
              <th>Until</th>
              <th>State</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            @for (session of sessions(); track session.uuid) {
              <tr>
                <td>
                  <strong>{{ session.target_name }}</strong>
                  @if (session.target_component) {
                    <span class="akd-muted"> · {{ session.target_component }}</span>
                  }
                  <span class="akd-muted"> :{{ session.target_port }}</span>
                  <span class="akd-badge">{{ session.target_kind }}</span>
                </td>
                <td>{{ session.user_email ?? 'API token' }}</td>
                <td class="akd-mono">{{ session.client_ip ?? '—' }}</td>
                <td>{{ session.started_at ?? session.created_at | date: 'short' }}</td>
                <td>
                  {{ session.authorized_until ? (session.authorized_until | date: 'short') : '—' }}
                </td>
                <td>
                  @if (session.active) {
                    <span class="akd-badge akd-badge--ok">open</span>
                  } @else {
                    <span class="akd-badge">{{ session.end_reason ?? 'closed' }}</span>
                  }
                </td>
                <td class="actions">
                  @if (session.active) {
                    <button
                      class="akd-btn akd-btn--ghost"
                      type="button"
                      (click)="cut(session)"
                      [disabled]="busy()"
                    >
                      Cut
                    </button>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }
  `,
  styles: [
    `
      .bar {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        margin-bottom: var(--space-4);
      }
      .toggle {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        font-size: var(--text-sm);
        color: var(--text-2);
      }
      .bar button {
        margin-left: auto;
      }
      .actions {
        text-align: right;
      }
    `,
  ],
})
export class TunnelSessionsTabComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly sessions = signal<PortForwardSession[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  /** Off by default: the live tunnels are the operational question. */
  protected includeClosed = false;

  constructor() {
    void this.load();
  }

  protected reload(): void {
    this.loading.set(true);
    void this.load();
  }

  private async load(): Promise<void> {
    this.error.set(null);
    try {
      const sessions = await fetchAll((cursor) =>
        this.api.client().listPortForwardSessions({
          active: !this.includeClosed,
          limit: 100,
          cursor,
        }),
      );
      this.sessions.set(sessions);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  /**
   * Cuts a live tunnel. Closing one's own needs nothing beyond the permission
   * that opened it; closing somebody else's is an administrative act and the
   * API says so — the refusal is surfaced rather than hidden behind a disabled
   * button, because who owns which session is the server's call, not ours.
   */
  protected async cut(session: PortForwardSession): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Cut the tunnel',
        message: `Cut the tunnel to "${session.target_name}"?`,
        confirmLabel: 'Cut',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().closePortForwardSession(session.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
