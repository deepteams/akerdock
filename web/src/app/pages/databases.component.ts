import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type Database = components['schemas']['Database'];
type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];
type Server = components['schemas']['Server'];

@Component({
  selector: 'app-databases',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Databases</h1>
        @if (!loading()) {
          <span class="akd-badge akd-badge--mono">{{ databases().length }}</span>
        }
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="toggleCreate()">
          <akd-icon name="plus" [size]="15" />
          {{ creating() ? 'Cancel' : 'New PostgreSQL database' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="db-name">Name</label>
            <input
              id="db-name"
              name="name"
              class="akd-input"
              required
              [(ngModel)]="name"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="db-project">Project</label>
            <div class="akd-select">
              <select
                id="db-project"
                name="project"
                class="akd-input"
                [(ngModel)]="projectUuid"
                (ngModelChange)="onProjectChange($event)"
                [disabled]="busy()"
              >
                <option value="" disabled>Choose a project…</option>
                @for (project of projects(); track project.uuid) {
                  <option [value]="project.uuid">{{ project.name }}</option>
                }
              </select>
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="db-environment">Environment</label>
            <div class="akd-select">
              <select
                id="db-environment"
                name="environment"
                class="akd-input"
                [(ngModel)]="environmentUuid"
                [disabled]="busy() || !projectUuid"
              >
                <option value="" disabled>
                  {{ projectUuid ? 'Choose an environment…' : 'Pick a project first' }}
                </option>
                @for (env of environments(); track env.uuid) {
                  <option [value]="env.uuid">{{ env.name }}</option>
                }
              </select>
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="db-server">Server</label>
            <div class="akd-select">
              <select
                id="db-server"
                name="server"
                class="akd-input"
                [(ngModel)]="serverUuid"
                [disabled]="busy()"
              >
                <option value="" disabled>Choose a server…</option>
                @for (server of servers(); track server.uuid) {
                  <option [value]="server.uuid">{{ server.name }} ({{ server.status }})</option>
                }
              </select>
            </div>
          </div>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy() || !valid()">
              {{ busy() ? 'Creating…' : 'Create database' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (databases().length === 0) {
        <akd-empty-state
          icon="database"
          title="No databases yet"
          message="Create a managed PostgreSQL — credentials are generated for you."
        >
          @if (!creating()) {
            <button class="akd-btn akd-btn--secondary" type="button" (click)="toggleCreate()">
              <akd-icon name="plus" [size]="15" />
              New PostgreSQL database
            </button>
          }
        </akd-empty-state>
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Managed databases of this team, with their desired and observed state
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Engine</th>
                <th scope="col">Desired</th>
                <th scope="col">Observed</th>
                <th scope="col">Public</th>
              </tr>
            </thead>
            <tbody>
              @for (db of databases(); track db.uuid) {
                <tr>
                  <td>
                    <a class="akd-mono" [routerLink]="['/databases', db.uuid]">{{ db.name }}</a>
                  </td>
                  <td>
                    @if (db.image) {
                      <span class="akd-badge akd-badge--accent akd-badge--mono">{{
                        db.image
                      }}</span>
                    } @else {
                      <span class="akd-muted">{{ db.engine }}</span>
                    }
                  </td>
                  <td><akd-status-badge domain="resource" [state]="db.desired_status" /></td>
                  <td><akd-status-badge domain="resource" [state]="db.observed_status" /></td>
                  <td>
                    @if (db.is_public) {
                      <span class="akd-badge akd-badge--mono"
                        >port {{ db.public_port ?? '?' }}</span
                      >
                    } @else {
                      <span class="akd-muted">no</span>
                    }
                  </td>
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
      .grow {
        flex: 1;
      }
      .create {
        margin-bottom: var(--space-5);
        max-width: 32rem;
      }
    `,
  ],
})
export class DatabasesComponent {
  private readonly api = inject(ApiService);

  protected readonly databases = signal<Database[]>([]);
  protected readonly projects = signal<Project[]>([]);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);

  protected name = '';
  protected projectUuid = '';
  protected environmentUuid = '';
  protected serverUuid = '';

  constructor() {
    void this.load();
    // The Projects drill-down lands here with ?create=1&project=&environment=
    // so the database is created where the user already was.
    const params = inject(ActivatedRoute).snapshot.queryParamMap;
    const project = params.get('project');
    const environment = params.get('environment');
    if (params.get('create')) {
      this.creating.set(true);
      void this.loadSelectors().then(async () => {
        if (!project) return;
        this.projectUuid = project;
        await this.onProjectChange(project);
        if (environment && this.environments().some((env) => env.uuid === environment)) {
          this.environmentUuid = environment;
        }
      });
    }
  }

  protected valid(): boolean {
    return !!(this.name.trim() && this.projectUuid && this.environmentUuid && this.serverUuid);
  }

  private async load(): Promise<void> {
    try {
      const databases = await fetchAll((cursor) =>
        this.api.client().listDatabases({ limit: 100, cursor }),
      );
      this.databases.set(databases);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected toggleCreate(): void {
    this.creating.set(!this.creating());
    if (this.creating()) void this.loadSelectors();
  }

  /** The form's selectors are real data, not free-text UUIDs. */
  private async loadSelectors(): Promise<void> {
    try {
      const [projects, servers] = await Promise.all([
        fetchAll((cursor) => this.api.client().listProjects({ limit: 100, cursor })),
        fetchAll((cursor) => this.api.client().listServers({ limit: 100, cursor })),
      ]);
      this.projects.set(projects);
      this.servers.set(servers);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async onProjectChange(projectUuid: string): Promise<void> {
    this.environmentUuid = '';
    this.environments.set([]);
    if (!projectUuid) return;
    try {
      const environments = await fetchAll((cursor) =>
        this.api.client().listEnvironments(projectUuid, { limit: 100, cursor }),
      );
      this.environments.set(environments);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createPostgresqlDatabase({
        name: this.name.trim(),
        project_uuid: this.projectUuid,
        environment_uuid: this.environmentUuid,
        server_uuid: this.serverUuid,
        image: 'postgres:16-alpine',
        postgres_user: 'postgres',
        is_public: false,
        public_access_mode: 'port_mapping',
        ssl_mode: 'disable',
        instant_start: false,
      });
      this.name = '';
      this.creating.set(false);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
