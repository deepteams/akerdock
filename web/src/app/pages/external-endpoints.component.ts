import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type ExternalEndpoint = components['schemas']['ExternalEndpoint'];
type Server = components['schemas']['Server'];
type PortForwardSession = components['schemas']['PortForwardSessionInfo'];

/**
 * Declared bastion targets (ADR-045): destinations outside the server that a
 * developer may tunnel to from their workstation — a managed database, a legacy
 * host on the private network.
 *
 * The address lives here, on the resource, and never in a tunnel request: that
 * is what keeps the CLI protocol addressless and stops a `write` holder from
 * scanning the private network. Declaring one draws a network boundary, so it
 * is an admin act, separate from using one.
 */
@Component({
  selector: 'app-external-endpoints',
  standalone: true,
  imports: [FormsModule, DatePipe, RouterLink, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>External endpoints</h2>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <akd-card title="Declare an endpoint">
        <form class="fields" (ngSubmit)="create()">
          <p class="intro">
            A single destination reached from one of your servers — no ranges, no wildcards. The CLI
            tunnels to it by name, and never sends an address of its own.
          </p>
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="ep-name">Name</label>
              <input
                id="ep-name"
                name="name"
                class="akd-input akd-input--mono"
                placeholder="e.g. prod-replica"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="ep-host">Host</label>
              <input
                id="ep-host"
                name="host"
                class="akd-input akd-input--mono"
                placeholder="e.g. db.internal or 10.0.0.7"
                [(ngModel)]="host"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="ep-port">Port</label>
              <input
                id="ep-port"
                name="port"
                type="number"
                class="akd-input akd-input--mono"
                [(ngModel)]="port"
                [disabled]="busy()"
                required
              />
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="ep-description">Description</label>
            <input
              id="ep-description"
              name="description"
              class="akd-input"
              placeholder="e.g. read replica of the billing database"
              [(ngModel)]="description"
              [disabled]="busy()"
            />
          </div>
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="ep-server">Egress server</label>
              <select
                id="ep-server"
                name="server"
                class="akd-input"
                [(ngModel)]="serverUuid"
                [disabled]="busy()"
                required
              >
                <option value="">Select a server…</option>
                @for (server of servers(); track server.uuid) {
                  <option [value]="server.uuid">{{ server.name }}</option>
                }
              </select>
              <span class="akd-field__hint">The tunnel is dialed from this server.</span>
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="ep-criticality">Access regime</label>
              <select
                id="ep-criticality"
                name="criticality"
                class="akd-input"
                [(ngModel)]="criticality"
                [disabled]="busy()"
              >
                <option value="sensitive">Sensitive — access must be requested</option>
                <option value="standard">Standard — open to anyone who may tunnel</option>
              </select>
              <span class="akd-field__hint">
                Sensitive requires a reason and a fresh second factor, for a bounded window.
              </span>
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="ep-window"
                >Longest access window (minutes)</label
              >
              <input
                id="ep-window"
                name="window"
                type="number"
                class="akd-input akd-input--mono"
                [(ngModel)]="maxGrantMinutes"
                [disabled]="busy() || criticality === 'standard'"
              />
              <span class="akd-field__hint"
                >Up to 480. Renewal is unlimited but always re-asks.</span
              >
            </div>
          </div>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              <akd-icon name="plus" [size]="15" />
              Declare endpoint
            </button>
          </div>
        </form>
      </akd-card>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (endpoints().length === 0) {
        <akd-empty-state
          icon="server"
          title="No external endpoint yet"
          text="Declare one to let your team tunnel to a database that AkerDock does not host."
        />
      } @else {
        <akd-card title="Declared endpoints">
          <table class="akd-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Destination</th>
                <th>Regime</th>
                <th>Your access</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              @for (endpoint of endpoints(); track endpoint.uuid) {
                <tr>
                  <td>
                    <a [routerLink]="['/external-endpoints', endpoint.uuid]">
                      <strong>{{ endpoint.name }}</strong>
                    </a>
                    @if (endpoint.description) {
                      <span class="akd-muted"> — {{ endpoint.description }}</span>
                    }
                  </td>
                  <td class="akd-mono">{{ endpoint.host }}:{{ endpoint.port }}</td>
                  <td>
                    <span
                      class="akd-badge"
                      [class.akd-badge--warn]="endpoint.criticality === 'sensitive'"
                    >
                      {{ endpoint.criticality }}
                    </span>
                  </td>
                  <td>
                    @if (endpoint.active_grant; as grant) {
                      <span class="akd-badge akd-badge--ok">
                        until {{ grant.expires_at | date: 'short' }}
                      </span>
                    } @else if (endpoint.criticality === 'sensitive') {
                      <a [routerLink]="['/external-endpoints', endpoint.uuid, 'request-access']">
                        Request access
                      </a>
                    } @else {
                      <span class="akd-muted">no grant needed</span>
                    }
                  </td>
                  <td class="actions">
                    <button
                      class="akd-btn akd-btn--ghost"
                      type="button"
                      (click)="remove(endpoint)"
                      [disabled]="busy()"
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </akd-card>
      }

      <akd-card title="Open tunnels">
        <p class="intro">
          Every tunnel your team currently holds — to a container as much as to a declared endpoint.
          Cutting one tells its holder why it went away.
        </p>
        @if (sessions().length === 0) {
          <p class="akd-muted">No tunnel open right now.</p>
        } @else {
          <table class="akd-table">
            <thead>
              <tr>
                <th>Target</th>
                <th>Who</th>
                <th>From</th>
                <th>Opened</th>
                <th>Until</th>
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
                    {{
                      session.authorized_until ? (session.authorized_until | date: 'short') : '—'
                    }}
                  </td>
                  <td class="actions">
                    <button
                      class="akd-btn akd-btn--ghost"
                      type="button"
                      (click)="cut(session)"
                      [disabled]="busy()"
                    >
                      Cut
                    </button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </akd-card>
    </div>
  `,
  styles: [
    `
      .akd-page > akd-card {
        margin-bottom: var(--space-5);
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .intro {
        margin: 0;
        color: var(--text-muted);
      }
      .row {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
        gap: var(--space-4);
      }
      .actions {
        text-align: right;
      }
    `,
  ],
})
export class ExternalEndpointsComponent {
  private readonly api = inject(ApiService);

  protected readonly endpoints = signal<ExternalEndpoint[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly sessions = signal<PortForwardSession[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected name = '';
  protected description = '';
  protected host = '';
  protected port = 5432;
  protected serverUuid = '';
  // Sensitive by default, like the API: declaring an external endpoint usually
  // means reaching a real database, so downgrading should be deliberate.
  protected criticality: 'standard' | 'sensitive' = 'sensitive';
  protected maxGrantMinutes = 240;

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const [endpoints, servers, sessions] = await Promise.all([
        this.api.client().listExternalEndpoints({ limit: 100 }),
        this.api.client().listServers({ limit: 100 }),
        this.api.client().listPortForwardSessions({ limit: 100 }),
      ]);
      this.endpoints.set(endpoints.data);
      this.servers.set(servers.data);
      this.sessions.set(sessions.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (!this.name.trim() || !this.host.trim() || !this.serverUuid) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createExternalEndpoint({
        name: this.name.trim(),
        description: this.description.trim() || undefined,
        host: this.host.trim(),
        port: this.port,
        server_uuid: this.serverUuid,
        criticality: this.criticality,
        max_grant_minutes: this.maxGrantMinutes,
      });
      this.name = '';
      this.description = '';
      this.host = '';
      this.port = 5432;
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(endpoint: ExternalEndpoint): Promise<void> {
    if (
      !confirm(`Delete "${endpoint.name}"? Open tunnels to it will stop at their next connection.`)
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteExternalEndpoint(endpoint.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * Cuts a live tunnel. Closing one's own needs nothing beyond the permission
   * that opened it; closing somebody else's is an administrative act and the
   * API says so — the refusal is surfaced rather than hidden behind a disabled
   * button, because who owns which session is the server's call, not ours.
   */
  protected async cut(session: PortForwardSession): Promise<void> {
    if (!confirm(`Cut the tunnel to "${session.target_name}"?`)) return;
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
