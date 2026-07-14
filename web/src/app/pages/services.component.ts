import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Service = components['schemas']['Service'];
type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];
type Server = components['schemas']['Server'];

const COMPOSE_PLACEHOLDER = `services:
  app:
    image: nginx:alpine
    expose: ["80"]
    environment:
      PUBLIC_URL: \${SERVICE_URL_APP}
`;

/**
 * Compose stacks: an inline compose file deployed as a multi-service
 * resource. The file is validated at the save — a stack that cannot deploy
 * is refused here, with the compose-spec §11 codes, not at deployment time.
 */
@Component({
  selector: 'app-services',
  standalone: true,
  imports: [FormsModule, RouterLink, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Services</h1>
        <button class="akd-btn" type="button" (click)="toggleCreate()">
          {{ creating() ? 'Cancel' : 'New compose stack' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label for="sv-name">Name</label>
            <input
              id="sv-name"
              name="name"
              class="akd-input"
              required
              [(ngModel)]="name"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label for="sv-project">Project</label>
            <select
              id="sv-project"
              name="project"
              class="akd-select"
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
          <div class="akd-field">
            <label for="sv-environment">Environment</label>
            <select
              id="sv-environment"
              name="environment"
              class="akd-select"
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
          <div class="akd-field">
            <label for="sv-server">Server</label>
            <select
              id="sv-server"
              name="server"
              class="akd-select"
              [(ngModel)]="serverUuid"
              [disabled]="busy()"
            >
              <option value="" disabled>Choose a server…</option>
              @for (server of servers(); track server.uuid) {
                <option [value]="server.uuid">{{ server.name }} ({{ server.status }})</option>
              }
            </select>
          </div>
          <div class="akd-field">
            <label for="sv-compose">
              Compose file (validated on save — build: is not allowed, magic variables
              SERVICE_* are)
            </label>
            <textarea
              id="sv-compose"
              name="compose"
              class="akd-textarea akd-mono"
              rows="14"
              [placeholder]="composePlaceholder"
              [(ngModel)]="composeContent"
              [disabled]="busy()"
            ></textarea>
          </div>
          <label class="check">
            <input
              type="checkbox"
              name="instantDeploy"
              [(ngModel)]="instantDeploy"
              [disabled]="busy()"
            />
            Deploy immediately after creation
          </label>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !valid()">
              {{ busy() ? 'Creating…' : 'Create stack' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (services().length === 0) {
        <div class="akd-empty">
          <p><strong>No compose stacks yet.</strong></p>
          <p class="akd-muted">
            Paste a docker-compose file and deploy it as a multi-service stack — one
            container per service, a domain per component.
          </p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Compose stacks of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Desired</th>
              <th scope="col">Observed</th>
            </tr>
          </thead>
          <tbody>
            @for (svc of services(); track svc.uuid) {
              <tr>
                <td>
                  <a [routerLink]="['/services', svc.uuid]">{{ svc.name }}</a>
                </td>
                <td><akd-status-badge domain="resource" [state]="svc.desired_status" /></td>
                <td><akd-status-badge domain="resource" [state]="svc.observed_status" /></td>
              </tr>
            }
          </tbody>
        </table>
      }
    </div>
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--akd-space-5);
        max-width: 44rem;
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
    `,
  ],
})
export class ServicesComponent {
  private readonly api = inject(ApiService);

  protected readonly composePlaceholder = COMPOSE_PLACEHOLDER;
  protected readonly services = signal<Service[]>([]);
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
  protected composeContent = '';
  protected instantDeploy = false;

  constructor() {
    void this.load();
  }

  protected valid(): boolean {
    return !!(
      this.name.trim() &&
      this.projectUuid &&
      this.environmentUuid &&
      this.serverUuid &&
      this.composeContent.trim()
    );
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listServices({ limit: 100 });
      this.services.set(page.data);
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

  private async loadSelectors(): Promise<void> {
    try {
      const [projects, servers] = await Promise.all([
        this.api.client().listProjects({ limit: 100 }),
        this.api.client().listServers({ limit: 100 }),
      ]);
      this.projects.set(projects.data);
      this.servers.set(servers.data.filter((server) => !server.is_build_server));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async onProjectChange(projectUuid: string): Promise<void> {
    this.environmentUuid = '';
    this.environments.set([]);
    if (!projectUuid) return;
    try {
      const page = await this.api.client().listEnvironments(projectUuid, { limit: 100 });
      this.environments.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createService({
        name: this.name.trim(),
        project_uuid: this.projectUuid,
        environment_uuid: this.environmentUuid,
        server_uuid: this.serverUuid,
        compose_content: this.composeContent,
        connect_to_predefined_network: false,
        instant_deploy: this.instantDeploy,
      });
      this.name = '';
      this.composeContent = '';
      this.creating.set(false);
      await this.load();
    } catch (err) {
      // The 422 carries the compose-spec §11 findings: shown verbatim, the
      // operator fixes the file where they wrote it.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
