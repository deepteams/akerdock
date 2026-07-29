import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type ExternalEndpoint = components['schemas']['ExternalEndpoint'];
type ExternalEndpointGrant = components['schemas']['ExternalEndpointGrant'];
type PortForwardSession = components['schemas']['PortForwardSessionInfo'];
type Server = components['schemas']['Server'];
type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];

/**
 * One bastion target, and the two things an operator actually comes here for:
 * **who currently holds access** to it, and **what is connected right now**.
 *
 * Declaring the endpoint draws a network boundary; the grant list is the audit
 * surface of ADR-045 §5 — who asked, why, with which factor, until when — and
 * revoking one tears down the tunnels it opened. That last part is the reason
 * revocation exists at all: a window closed on somebody already connected would
 * mean nothing.
 */
@Component({
  selector: 'app-external-endpoint-detail',
  standalone: true,
  imports: [FormsModule, DatePipe, RouterLink, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <a class="akd-btn akd-btn--ghost" routerLink="/external-endpoints">
          <akd-icon name="arrow-left" [size]="15" />
          Tunnels
        </a>
        <h2>{{ endpoint()?.name ?? 'Endpoint' }}</h2>
        @if (endpoint(); as ep) {
          <span class="akd-badge" [class.akd-badge--warn]="ep.criticality === 'sensitive'">
            {{ ep.criticality }}
          </span>
        }
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (endpoint(); as ep) {
        <akd-card title="Destination">
          <form class="fields" (ngSubmit)="save()">
            <p class="intro">
              Changing the address changes what your team can reach from
              <strong>{{ serverName() }}</strong> — it is audited as such.
            </p>
            <div class="row">
              <div class="akd-field">
                <label class="akd-field__label" for="ep-name">Name</label>
                <input
                  id="ep-name"
                  name="name"
                  class="akd-input akd-input--mono"
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
                  @for (server of servers(); track server.uuid) {
                    <option [value]="server.uuid">{{ server.name }}</option>
                  }
                </select>
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
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="ep-window">Longest access window (min)</label>
                <input
                  id="ep-window"
                  name="window"
                  type="number"
                  class="akd-input akd-input--mono"
                  [(ngModel)]="maxGrantMinutes"
                  [disabled]="busy() || criticality === 'standard'"
                />
              </div>
            </div>
            <div class="row">
              <div class="akd-field">
                <label class="akd-field__label" for="ep-project">Related project</label>
                <select
                  id="ep-project"
                  name="project"
                  class="akd-input"
                  [ngModel]="projectUuid"
                  (ngModelChange)="onProjectChange($event)"
                  [disabled]="busy()"
                >
                  <option value="">Not related to a project</option>
                  @for (project of projects(); track project.uuid) {
                    <option [value]="project.uuid">{{ project.name }}</option>
                  }
                </select>
                <span class="akd-field__hint">
                  Descriptive only: it records what this destination is for, and is not an access
                  boundary (ADR-047).
                </span>
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="ep-environment">…and environment</label>
                <select
                  id="ep-environment"
                  name="environment"
                  class="akd-input"
                  [(ngModel)]="environmentUuid"
                  [disabled]="busy() || !projectUuid"
                >
                  <option value="">Every environment of the project</option>
                  @for (environment of environments(); track environment.uuid) {
                    <option [value]="environment.uuid">{{ environment.name }}</option>
                  }
                </select>
              </div>
            </div>
            <div class="actions-row">
              <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                {{ busy() ? 'Saving…' : 'Save changes' }}
              </button>
              @if (ep.criticality === 'sensitive') {
                <a
                  class="akd-btn akd-btn--ghost"
                  [routerLink]="['/external-endpoints', ep.uuid, 'request-access']"
                >
                  Request access
                </a>
              }
            </div>
          </form>
        </akd-card>

        <akd-card title="Open tunnels">
          <p class="intro">
            What is connected to this endpoint right now. Cutting one tells the developer why it
            went away — a tunnel that dies in silence is read as a bug.
          </p>
          @if (sessions().length === 0) {
            <p class="akd-muted">No tunnel open to this endpoint.</p>
          } @else {
            <table class="akd-table">
              <thead>
                <tr>
                  <th>Who</th>
                  <th>From</th>
                  <th>Opened</th>
                  <th>Authorized until</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                @for (session of sessions(); track session.uuid) {
                  <tr>
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

        <akd-card title="Access grants">
          <p class="intro">
            Who asked for access, why, and for how long. Revoking a live grant closes the window
            <em>and</em> tears down the tunnels it opened.
          </p>
          @if (grants().length === 0) {
            <p class="akd-muted">Nobody has requested access to this endpoint yet.</p>
          } @else {
            <table class="akd-table">
              <thead>
                <tr>
                  <th>Who</th>
                  <th>Reason</th>
                  <th>Factor</th>
                  <th>Requested</th>
                  <th>Status</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                @for (grant of grants(); track grant.uuid) {
                  <tr>
                    <td>{{ grant.user_email ?? '—' }}</td>
                    <td>
                      {{ grant.reason }}
                      @if (grant.renewed) {
                        <span class="akd-muted"> (renewed)</span>
                      }
                    </td>
                    <td>{{ grant.factor }}</td>
                    <td>{{ grant.requested_at | date: 'short' }}</td>
                    <td>
                      @if (grant.revoked_at) {
                        <span class="akd-badge">revoked</span>
                      } @else if (isLive(grant)) {
                        <span class="akd-badge akd-badge--ok">
                          until {{ grant.expires_at | date: 'short' }}
                        </span>
                      } @else {
                        <span class="akd-badge">expired</span>
                      }
                    </td>
                    <td class="actions">
                      @if (!grant.revoked_at && isLive(grant)) {
                        <button
                          class="akd-btn akd-btn--ghost"
                          type="button"
                          (click)="revoke(grant)"
                          [disabled]="busy()"
                        >
                          Revoke
                        </button>
                      }
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      } @else if (!error()) {
        <p class="akd-muted">Loading…</p>
      }
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
        margin: 0 0 var(--space-3);
        color: var(--text-muted);
      }
      .row {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
        gap: var(--space-4);
      }
      .actions-row {
        display: flex;
        gap: var(--space-3);
        align-items: center;
      }
      .actions {
        text-align: right;
      }
    `,
  ],
})
export class ExternalEndpointDetailComponent {
  private readonly api = inject(ApiService);

  /** Route parameter, bound by withComponentInputBinding(). */
  readonly uuid = input.required<string>();

  protected readonly endpoint = signal<ExternalEndpoint | null>(null);
  protected readonly grants = signal<ExternalEndpointGrant[]>([]);
  protected readonly sessions = signal<PortForwardSession[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly projects = signal<Project[]>([]);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected name = '';
  protected description = '';
  protected host = '';
  protected port = 5432;
  protected serverUuid = '';
  protected criticality: 'standard' | 'sensitive' = 'sensitive';
  protected maxGrantMinutes = 240;
  protected projectUuid = '';
  protected environmentUuid = '';

  protected readonly serverName = computed(
    () => this.servers().find((s) => s.uuid === this.serverUuid)?.name ?? 'its egress server',
  );

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const [endpoint, grants, sessions, servers, projects] = await Promise.all([
        this.api.client().getExternalEndpoint(this.uuid()),
        this.api.client().listExternalEndpointGrants(this.uuid(), { limit: 100 }),
        this.api
          .client()
          .listPortForwardSessions({ external_endpoint_uuid: this.uuid(), limit: 100 }),
        this.api.client().listServers({ limit: 100 }),
        this.api.client().listProjects({ limit: 100 }),
      ]);
      this.endpoint.set(endpoint);
      this.grants.set(grants.data);
      this.sessions.set(sessions.data);
      this.servers.set(servers.data);
      this.projects.set(projects.data);
      this.fillForm(endpoint);
      if (endpoint.project_uuid) {
        await this.loadEnvironments(endpoint.project_uuid);
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  private fillForm(endpoint: ExternalEndpoint): void {
    this.name = endpoint.name;
    this.description = endpoint.description ?? '';
    this.host = endpoint.host;
    this.port = endpoint.port;
    this.serverUuid = endpoint.server_uuid;
    this.criticality = endpoint.criticality;
    this.maxGrantMinutes = endpoint.max_grant_minutes;
    this.projectUuid = endpoint.project_uuid ?? '';
    this.environmentUuid = endpoint.environment_uuid ?? '';
  }

  protected async onProjectChange(uuid: string): Promise<void> {
    this.projectUuid = uuid;
    // An environment is scoped by its project, so dropping the project drops
    // the environment with it — the API refuses the other combination anyway.
    this.environmentUuid = '';
    this.environments.set([]);
    if (uuid) {
      await this.loadEnvironments(uuid);
    }
  }

  private async loadEnvironments(projectUuid: string): Promise<void> {
    try {
      const page = await this.api.client().listEnvironments(projectUuid, { limit: 100 });
      this.environments.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected isLive(grant: ExternalEndpointGrant): boolean {
    return !grant.revoked_at && new Date(grant.expires_at).getTime() > Date.now();
  }

  protected async save(): Promise<void> {
    if (!this.name.trim() || !this.host.trim() || !this.serverUuid) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const updated = await this.api.client().updateExternalEndpoint(this.uuid(), {
        name: this.name.trim(),
        description: this.description.trim() || undefined,
        host: this.host.trim(),
        port: this.port,
        server_uuid: this.serverUuid,
        project_uuid: this.projectUuid || undefined,
        environment_uuid: this.environmentUuid || undefined,
        criticality: this.criticality,
        max_grant_minutes: this.maxGrantMinutes,
      });
      this.endpoint.set(updated);
      this.fillForm(updated);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revoke(grant: ExternalEndpointGrant): Promise<void> {
    if (
      !confirm(
        `Revoke ${grant.user_email ?? 'this'} access? Their open tunnels to this endpoint are cut immediately.`,
      )
    ) {
      return;
    }
    await this.mutate(() => this.api.client().revokeExternalEndpointGrant(grant.uuid));
  }

  protected async cut(session: PortForwardSession): Promise<void> {
    if (!confirm(`Cut the tunnel opened by ${session.user_email ?? 'an API token'}?`)) return;
    await this.mutate(() => this.api.client().closePortForwardSession(session.uuid));
  }

  /** Runs a mutation and reloads, so the two tables never disagree. */
  private async mutate(action: () => Promise<unknown>): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      await action();
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
