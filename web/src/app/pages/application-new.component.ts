import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
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
  imports: [
    FormsModule,
    RouterLink,
    ApplicationConfigFieldsComponent,
    CardComponent,
    IconComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div class="title">
          <a
            class="akd-iconbtn akd-iconbtn--bordered"
            routerLink="/applications"
            aria-label="Back to applications"
          >
            <akd-icon name="arrow-left" [size]="15" />
          </a>
          <h1>New application</h1>
        </div>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <form class="form" (ngSubmit)="create()">
        <akd-card title="Placement">
          <div class="fields">
            <div class="akd-field">
              <label class="akd-field__label" for="an-project">Project</label>
              <div class="akd-select">
                <select
                  id="an-project"
                  name="project"
                  class="akd-input"
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
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="an-environment">Environment</label>
              <div class="akd-select">
                <select
                  id="an-environment"
                  name="environment"
                  class="akd-input"
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
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="an-server">Server</label>
              <div class="akd-select">
                <select
                  id="an-server"
                  name="server"
                  class="akd-input"
                  [(ngModel)]="form.serverUuid"
                  [disabled]="busy()"
                >
                  <option value="" disabled>Choose a server…</option>
                  @for (server of servers(); track server.uuid) {
                    <option [value]="server.uuid">{{ server.name }} ({{ server.status }})</option>
                  }
                </select>
              </div>
            </div>
          </div>
        </akd-card>

        <akd-card title="Source type">
          <div class="sources" role="radiogroup" aria-label="Source type">
            @for (s of sourceTypes; track s.id) {
              <label class="source" [class.source--active]="form.sourceType === s.id">
                <input
                  type="radio"
                  name="sourceType"
                  [value]="s.id"
                  [(ngModel)]="form.sourceType"
                  [disabled]="busy()"
                />
                <strong>{{ s.label }}</strong>
                <span class="source__hint">{{ s.hint }}</span>
              </label>
            }
          </div>
        </akd-card>

        @if (form.sourceType === 'git' && githubApps().length > 0) {
          <akd-card title="GitHub App (optional)">
            <div class="fields">
              <div class="akd-field">
                <label class="akd-field__label" for="an-ghapp">Source</label>
                <div class="akd-select">
                  <select
                    id="an-ghapp"
                    name="githubApp"
                    class="akd-input"
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
              </div>
              @if (form.githubAppUuid) {
                <div class="akd-field">
                  <label class="akd-field__label" for="an-ghrepo">Repository</label>
                  <div class="akd-select">
                    <select
                      id="an-ghrepo"
                      name="githubRepo"
                      class="akd-input"
                      [(ngModel)]="form.repositoryFullName"
                      (ngModelChange)="onRepositoryChange($event)"
                      [disabled]="busy()"
                    >
                      <option value="" disabled>
                        {{
                          repositories().length ? 'Choose a repository…' : 'Loading repositories…'
                        }}
                      </option>
                      @for (repo of repositories(); track repo.uuid) {
                        <option [value]="repo.full_name">{{ repo.full_name }}</option>
                      }
                    </select>
                  </div>
                </div>
              }
            </div>
          </akd-card>
        }

        <app-application-config-fields
          [form]="form"
          [sourceType]="form.sourceType"
          [githubApp]="!!form.githubAppUuid"
          [registries]="registries()"
          [privateKeys]="privateKeys()"
          [busy]="busy()"
        />

        <label class="akd-check">
          <input
            type="checkbox"
            name="instantDeploy"
            [(ngModel)]="form.instantDeploy"
            [disabled]="busy()"
          />
          Deploy immediately after creation
        </label>

        <div class="actions">
          <button
            class="akd-btn akd-btn--primary"
            type="submit"
            [disabled]="busy() || problem() !== null"
          >
            <akd-icon name="plus" [size]="15" />
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
      .title {
        display: flex;
        align-items: center;
        gap: var(--space-3);
      }
      .form {
        max-width: 720px;
        display: grid;
        gap: var(--space-4);
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .sources {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
        gap: var(--space-2);
      }
      .source {
        display: grid;
        gap: var(--space-1);
        padding: var(--space-3);
        background: var(--bg-2);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-2);
        cursor: pointer;
        font-size: var(--text-sm);
        color: var(--text-1);
        transition: border-color var(--dur-1) var(--ease-out);
      }
      .source--active {
        border-color: var(--accent-border);
        background: var(--accent-dim);
      }
      .source__hint {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .actions {
        display: flex;
        align-items: center;
        gap: var(--space-3);
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
    // The Projects drill-down lands here with ?project=&environment= so the
    // resource is created where the user already was.
    const params = inject(ActivatedRoute).snapshot.queryParamMap;
    const project = params.get('project');
    const environment = params.get('environment');
    if (project) {
      this.form.projectUuid = project;
      void this.onProjectChange(project).then(() => {
        if (environment && this.environments().some((env) => env.uuid === environment)) {
          this.form.environmentUuid = environment;
        }
      });
    }
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
      //
      // replaceUrl: the submitted form is not a place to go back to — neither
      // for the browser's back button nor for the detail page's back arrow,
      // which would otherwise offer to return to a creation form for an
      // application that now exists.
      await this.router.navigate(['/applications', created.uuid], { replaceUrl: true });
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
