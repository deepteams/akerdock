import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { CardComponent } from '../../../ui/card/card.component';
import { DrawerComponent } from '../../../ui/drawer/drawer.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { fetchAll } from '../../core/pagination';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import type { components } from '../../../api/schema';

type ExternalEndpoint = components['schemas']['ExternalEndpoint'];
type Server = components['schemas']['Server'];
type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];

/**
 * The declared bastion targets (ADR-045) — the tunnels that CAN be opened, as
 * opposed to the ones currently open.
 *
 * The address lives here, on the resource, and never in a tunnel request: that
 * is what keeps the CLI protocol addressless and stops a `write` holder from
 * scanning the private network. Declaring one draws a network boundary, so it
 * is an admin act, separate from using one.
 */
@Component({
  selector: 'app-tunnel-endpoints-tab',
  standalone: true,
  imports: [
    FormsModule,
    DatePipe,
    RouterLink,
    CardComponent,
    DrawerComponent,
    EmptyStateComponent,
    IconComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="bar">
      <button class="akd-btn akd-btn--primary" type="button" (click)="openNew()">
        <akd-icon name="plus" [size]="15" />
        Declare endpoint
      </button>
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (endpoints().length === 0) {
      <akd-empty-state
        icon="cable"
        title="No external endpoint yet"
        message="Declare one to let your team tunnel to a database that AkerDock does not host."
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

    <akd-drawer [open]="showNew()" title="Declare an endpoint" (closed)="closeNew()">
      <form id="endpoint-form" class="fields" (ngSubmit)="create()">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <p class="intro">
          A single destination reached from one of your servers — no ranges, no wildcards. The CLI
          tunnels to it by name, and never sends an address of its own.
        </p>
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
        <div class="row">
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
        <div class="akd-field">
          <label class="akd-field__label" for="ep-server">Egress server</label>
          <div class="akd-select">
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
          </div>
          <span class="akd-field__hint">The tunnel is dialed from this server.</span>
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="ep-criticality">Access regime</label>
          <div class="akd-select">
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
          <span class="akd-field__hint">
            Sensitive requires a reason and a fresh second factor, for a bounded window.
          </span>
        </div>
        @if (criticality === 'sensitive') {
          <div class="akd-field">
            <label class="akd-field__label" for="ep-window">Longest access window (minutes)</label>
            <input
              id="ep-window"
              name="window"
              type="number"
              class="akd-input akd-input--mono"
              [(ngModel)]="maxGrantMinutes"
              [disabled]="busy()"
            />
            <span class="akd-field__hint">
              Up to 480. Renewal is unlimited but always re-asks.
            </span>
          </div>
        }
        <div class="akd-field">
          <label class="akd-field__label" for="ep-project">Restricted to project</label>
          <div class="akd-select">
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
          </div>
          <span class="akd-field__hint">
            Descriptive only: it records what this destination is for. It is <em>not</em> an access
            boundary — anyone on the team who may tunnel can reach it (ADR-047).
          </span>
        </div>
        @if (projectUuid) {
          <div class="akd-field">
            <label class="akd-field__label" for="ep-environment">…and environment</label>
            <div class="akd-select">
              <select
                id="ep-environment"
                name="environment"
                class="akd-input"
                [(ngModel)]="environmentUuid"
                [disabled]="busy()"
              >
                <option value="">Every environment of the project</option>
                @for (environment of environments(); track environment.uuid) {
                  <option [value]="environment.uuid">{{ environment.name }}</option>
                }
              </select>
            </div>
          </div>
        }
      </form>
      <div drawer-footer>
        <button
          class="akd-btn akd-btn--ghost"
          type="button"
          (click)="closeNew()"
          [disabled]="busy()"
        >
          Cancel
        </button>
        <button
          class="akd-btn akd-btn--primary"
          type="submit"
          form="endpoint-form"
          [disabled]="busy() || !valid()"
        >
          <akd-icon name="plus" [size]="15" />
          Declare endpoint
        </button>
      </div>
    </akd-drawer>
  `,
  styles: [
    `
      .bar {
        display: flex;
        justify-content: flex-end;
        margin-bottom: var(--space-4);
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
        grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
        gap: var(--space-4);
      }
      .actions {
        text-align: right;
      }
    `,
  ],
})
export class TunnelEndpointsTabComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly endpoints = signal<ExternalEndpoint[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly projects = signal<Project[]>([]);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly showNew = signal(false);
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
  protected projectUuid = '';
  protected environmentUuid = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const [endpoints, servers, projects] = await Promise.all([
        fetchAll((cursor) => this.api.client().listExternalEndpoints({ limit: 100, cursor })),
        fetchAll((cursor) => this.api.client().listServers({ limit: 100, cursor })),
        fetchAll((cursor) => this.api.client().listProjects({ limit: 100, cursor })),
      ]);
      this.endpoints.set(endpoints);
      this.servers.set(servers);
      this.projects.set(projects);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected openNew(): void {
    this.error.set(null);
    this.showNew.set(true);
  }

  /** Closing discards the draft: a half-filled form kept around is a trap. */
  protected closeNew(): void {
    this.showNew.set(false);
    this.resetForm();
  }

  private resetForm(): void {
    this.name = '';
    this.description = '';
    this.host = '';
    this.port = 5432;
    this.serverUuid = '';
    this.criticality = 'sensitive';
    this.maxGrantMinutes = 240;
    this.projectUuid = '';
    this.environmentUuid = '';
    this.environments.set([]);
  }

  /** The three fields the API cannot default: everything else has one. */
  protected valid(): boolean {
    return !!this.name.trim() && !!this.host.trim() && !!this.serverUuid;
  }

  protected async onProjectChange(uuid: string): Promise<void> {
    this.projectUuid = uuid;
    // An environment is scoped by its project, so dropping the project drops
    // the environment with it — the API refuses the other combination anyway.
    this.environmentUuid = '';
    this.environments.set([]);
    if (!uuid) return;
    try {
      const environments = await fetchAll((cursor) =>
        this.api.client().listEnvironments(uuid, { limit: 100, cursor }),
      );
      this.environments.set(environments);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async create(): Promise<void> {
    if (!this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createExternalEndpoint({
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
      this.showNew.set(false);
      this.resetForm();
      await this.load();
    } catch (err) {
      // The drawer stays open on failure — a name collision is fixed in place,
      // not by retyping the whole form.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(endpoint: ExternalEndpoint): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the endpoint',
        message: `Delete "${endpoint.name}"? Open tunnels to it will stop at their next connection.`,
        confirmLabel: 'Delete',
      }))
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
}
