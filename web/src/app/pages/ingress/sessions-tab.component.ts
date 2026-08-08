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

type IngressSession = components['schemas']['IngressTunnelSessionInfo'];

/**
 * The ingress attach sessions (ADR-060): who is publishing their machine right
 * now, on which URL. Liveness is agent-reported — the socket lives on the
 * ingress server — so `active` reflects the last report, not a control-plane
 * connection. Cutting one names a reason the CLI prints, and a policy close is
 * not re-dialed.
 */
@Component({
  selector: 'app-ingress-sessions-tab',
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
        icon="globe"
        title="No ingress tunnel open right now"
        [message]="
          includeClosed
            ? 'Nothing has been relayed recently.'
            : 'Nobody on the team is publishing their machine at the moment.'
        "
      />
    } @else {
      <akd-card [title]="includeClosed ? 'Ingress sessions' : 'Live ingress tunnels'">
        <table class="akd-table">
          <thead>
            <tr>
              <th>Endpoint</th>
              <th>Who</th>
              <th>From</th>
              <th>Since</th>
              <th>State</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            @for (session of sessions(); track session.uuid) {
              <tr>
                <td>
                  <strong>{{ session.endpoint_name ?? '—' }}</strong>
                  @if (session.fqdn) {
                    <span class="akd-muted akd-mono"> · {{ session.fqdn }}</span>
                  }
                </td>
                <td>{{ session.user_email ?? 'API token' }}</td>
                <td class="akd-mono">{{ session.client_ip ?? '—' }}</td>
                <td>{{ session.started_at ?? session.created_at | date: 'short' }}</td>
                <td>
                  @if (session.active) {
                    <span class="akd-badge akd-badge--ok">live</span>
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
export class IngressSessionsTabComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly sessions = signal<IngressSession[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

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
        this.api.client().listIngressTunnelSessions({
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

  protected async cut(session: IngressSession): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Cut the ingress tunnel',
        message: `Cut the tunnel of "${session.endpoint_name ?? session.fqdn}"? The developer's session ends and the URL goes offline until they reconnect.`,
        confirmLabel: 'Cut',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().closeIngressTunnelSession(session.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
