import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';
import { ApplicationConfigFieldsComponent } from './application/config-fields.component';
import {
  createFormProblem,
  createRequestFromForm,
  emptyCreateForm,
} from './application/application-form';

type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];
type Server = components['schemas']['Server'];
type RegistryCredential = components['schemas']['RegistryCredential'];
type PrivateKey = components['schemas']['PrivateKey'];
type SourceType = components['schemas']['Application']['source_type'];
type GithubApp = components['schemas']['GithubApp'];
type GitRepository = components['schemas']['GitRepository'];

@Component({
  selector: 'app-application-new',
  standalone: true,
  imports: [FormsModule, RouterLink, ApplicationConfigFieldsComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div>
          <a routerLink="/applications" class="back">← Applications</a>
          <h1>New application</h1>
        </div>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <form class="form" (ngSubmit)="create()">
        <fieldset class="group">
          <legend>Placement</legend>
          <div class="akd-field">
            <label for="an-project">Project</label>
            <select
              id="an-project"
              name="project"
              class="akd-select"
              [(ngModel)]="form.projectUuid"
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
            <label for="an-environment">Environment</label>
            <select
              id="an-environment"
              name="environment"
              class="akd-select"
              [(ngModel)]="form.environmentUuid"
              [disabled]="busy() || !form.projectUuid"
            >
              <option value="" disabled>
                {{ form.projectUuid ? 'Choose an environment…' : 'Pick a project first' }}
              </option>
              @for (env of environments(); track env.uuid) {
                <option [value]="env.uuid">{{ env.name }}</option>
              }
            </select>
          </div>
          <div class="akd-field">
            <label for="an-server">Server</label>
            <select
              id="an-server"
              name="server"
              class="akd-select"
              [(ngModel)]="form.serverUuid"
              [disabled]="busy()"
            >
              <option value="" disabled>Choose a server…</option>
              @for (server of servers(); track server.uuid) {
                <option [value]="server.uuid">{{ server.name }} ({{ server.status }})</option>
              }
            </select>
          </div>
        </fieldset>

        <fieldset class="group">
          <legend>Source type</legend>
          <div class="sources" role="radiogroup" aria-label="Source type">
            @for (s of sourceTypes; track s.id) {
              <label class="source" [class.active]="form.sourceType === s.id">
                <input
                  type="radio"
                  name="sourceType"
                  [value]="s.id"
                  [(ngModel)]="form.sourceType"
                  [disabled]="busy()"
                />
                <strong>{{ s.label }}</strong>
                <span class="akd-muted">{{ s.hint }}</span>
              </label>
            }
          </div>
        </fieldset>

        @if (form.sourceType === 'git' && githubApps().length > 0) {
          <fieldset class="group">
            <legend>GitHub App (optional)</legend>
            <div class="akd-field">
              <label for="an-ghapp">Source</label>
              <select
                id="an-ghapp"
                name="githubApp"
                class="akd-select"
                [(ngModel)]="form.githubAppUuid"
                (ngModelChange)="onGithubAppChange($event)"
                [disabled]="busy()"
              >
                <option value="">Manual URL (public or deploy key)</option>
                @for (app of githubApps(); track app.uuid) {
                  <option [value]="app.uuid" [disabled]="!app.is_installed">
                    {{ app.name }}{{ app.is_installed ? '' : ' (not installed)' }}
                  </option>
                }
              </select>
            </div>
            @if (form.githubAppUuid) {
              <div class="akd-field">
                <label for="an-ghrepo">Repository</label>
                <select
                  id="an-ghrepo"
                  name="githubRepo"
                  class="akd-select"
                  [(ngModel)]="form.repositoryFullName"
                  (ngModelChange)="onRepositoryChange($event)"
                  [disabled]="busy()"
                >
                  <option value="" disabled>
                    {{ repositories().length ? 'Choose a repository…' : 'Loading repositories…' }}
                  </option>
                  @for (repo of repositories(); track repo.uuid) {
                    <option [value]="repo.full_name">{{ repo.full_name }}</option>
                  }
                </select>
              </div>
            }
          </fieldset>
        }

        <app-application-config-fields
          [form]="form"
          [sourceType]="form.sourceType"
          [registries]="registries()"
          [privateKeys]="privateKeys()"
          [busy]="busy()"
        />

        <label class="check">
          <input
            type="checkbox"
            name="instantDeploy"
            [(ngModel)]="form.instantDeploy"
            [disabled]="busy()"
          />
          Deploy immediately after creation
        </label>

        <div class="actions">
          <button class="akd-btn" type="submit" [disabled]="busy() || problem() !== null">
            {{ busy() ? 'Creating…' : 'Create application' }}
          </button>
          @if (problem(); as message) {
            <span class="akd-muted">{{ message }}</span>
          }
        </div>
      </form>
    </div>
  `,
  styles: [
    `
      .back {
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
      }
      .back:hover {
        text-decoration: underline;
      }
      .form {
        max-width: 44rem;
        display: grid;
        gap: var(--akd-space-3);
      }
      .group {
        margin: 0;
        padding: var(--akd-space-3) var(--akd-space-4) var(--akd-space-4);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        display: grid;
        gap: var(--akd-space-3);
      }
      legend {
        padding: 0 var(--akd-space-2);
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.04em;
      }
      .sources {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
        gap: var(--akd-space-2);
      }
      .source {
        display: grid;
        gap: var(--akd-space-1);
        padding: var(--akd-space-3);
        border: 1px solid var(--akd-border-input);
        border-radius: var(--akd-radius-lg);
        cursor: pointer;
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .source.active {
        border-color: var(--akd-focus-ring);
        background: var(--akd-surface-hover);
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .actions {
        display: flex;
        align-items: center;
        gap: var(--akd-space-3);
      }
    `,
  ],
})
export class ApplicationNewComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly sourceTypes: readonly { id: SourceType; label: string; hint: string }[] = [
    { id: 'docker_image', label: 'Docker image', hint: 'Deploy a prebuilt image from a registry' },
    { id: 'dockerfile', label: 'Dockerfile', hint: 'Build from an inline Dockerfile' },
    { id: 'git', label: 'Git repository', hint: 'Clone, build and deploy from a repository' },
  ];

  protected readonly projects = signal<Project[]>([]);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly registries = signal<RegistryCredential[]>([]);
  protected readonly privateKeys = signal<PrivateKey[]>([]);
  protected readonly githubApps = signal<GithubApp[]>([]);
  protected readonly repositories = signal<GitRepository[]>([]);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected form = emptyCreateForm();

  constructor() {
    void this.loadSelectors();
  }

  protected problem(): string | null {
    return createFormProblem(this.form);
  }

  /** The form's selectors are real data, not free-text UUIDs. */
  private async loadSelectors(): Promise<void> {
    const client = this.api.client();
    try {
      const [projects, servers, registries, keys, apps] = await Promise.all([
        client.listProjects({ limit: 100 }),
        client.listServers({ limit: 100 }),
        client.listRegistryCredentials({ limit: 100 }),
        client.listPrivateKeys({ limit: 100 }),
        client.listGithubApps({ limit: 100 }),
      ]);
      this.projects.set(projects.data);
      // A build server builds; it must not host applications (§3.4).
      this.servers.set(servers.data.filter((server) => !server.is_build_server));
      this.registries.set(registries.data);
      this.privateKeys.set(keys.data);
      this.githubApps.set(apps.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async onProjectChange(projectUuid: string): Promise<void> {
    this.form.environmentUuid = '';
    this.environments.set([]);
    if (!projectUuid) return;
    try {
      const page = await this.api.client().listEnvironments(projectUuid, { limit: 100 });
      this.environments.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async onGithubAppChange(uuid: string): Promise<void> {
    this.form.repositoryFullName = '';
    this.repositories.set([]);
    if (!uuid) return;
    try {
      const page = await this.api.client().listGithubAppRepositories(uuid);
      this.repositories.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /** Picking a repository prefills the branch with its default. */
  protected onRepositoryChange(fullName: string): void {
    const repo = this.repositories().find((r) => r.full_name === fullName);
    if (repo?.default_branch && !this.form.gitBranch.trim()) {
      this.form.gitBranch = repo.default_branch;
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || this.problem() !== null) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const created = await this.api.client().createApplication(createRequestFromForm(this.form));
      // Land on the application just created: configuring and deploying it is
      // the natural next step, and the detail page is where both live.
      await this.router.navigate(['/applications', created.uuid]);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
