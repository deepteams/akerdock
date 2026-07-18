import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import { BreadcrumbComponent, type Crumb } from '../../ui/breadcrumb/breadcrumb.component';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];

@Component({
  selector: 'app-project-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    BreadcrumbComponent,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <div class="crumbs">
        <akd-breadcrumb [items]="crumbs()" />
      </div>
      <header class="akd-bar">
        <div class="title">
          <a
            class="akd-iconbtn akd-iconbtn--bordered"
            routerLink="/projects"
            aria-label="Back to projects"
          >
            <akd-icon name="arrow-left" [size]="16" />
          </a>
          <h1 class="title__name">{{ project()?.name ?? '…' }}</h1>
        </div>
        <button
          class="akd-btn akd-btn--danger"
          type="button"
          [disabled]="busy()"
          (click)="remove()"
        >
          <akd-icon name="trash-2" [size]="15" />
          Delete project
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (project(); as p) {
        <akd-card title="Details" class="section">
          <form class="row" (ngSubmit)="save()">
            <div class="akd-field grow">
              <label class="akd-field__label" for="pd-name">Name</label>
              <input
                id="pd-name"
                name="name"
                class="akd-input akd-input--mono"
                required
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field grow">
              <label class="akd-field__label" for="pd-description">Description</label>
              <input
                id="pd-description"
                name="description"
                class="akd-input"
                [(ngModel)]="description"
                [disabled]="busy()"
              />
            </div>
            <button
              class="akd-btn akd-btn--secondary"
              type="submit"
              [disabled]="busy() || !name.trim()"
            >
              Save
            </button>
          </form>
        </akd-card>

        <akd-card title="Environments" [padded]="false" class="section">
          <form card-actions class="envform" (ngSubmit)="createEnvironment()">
            <input
              class="akd-input akd-input--mono"
              name="envName"
              placeholder="e.g. production, staging"
              aria-label="New environment name"
              [(ngModel)]="envName"
              [disabled]="busy()"
            />
            <button
              class="akd-btn akd-btn--primary akd-btn--sm"
              type="submit"
              [disabled]="busy() || !envName.trim()"
            >
              <akd-icon name="plus" [size]="14" />
              Add
            </button>
          </form>

          @if (environments().length === 0) {
            <akd-empty-state
              icon="square-dashed"
              title="No environments yet"
              message="Resources live inside an environment — create one to start deploying."
            />
          } @else {
            <table class="akd-table akd-table--clickable">
              <caption class="sr-only">
                Environments of this project
              </caption>
              <thead>
                <tr>
                  <th scope="col">Environment</th>
                  <th scope="col">Description</th>
                  <th scope="col" class="right">Resources</th>
                  <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (env of environments(); track env.uuid) {
                  <tr (click)="openEnvironment(env)">
                    <td>
                      @if (editing() === env.uuid) {
                        <form
                          class="rename"
                          (ngSubmit)="saveEnvironment(env)"
                          (click)="$event.stopPropagation()"
                        >
                          <input
                            class="akd-input akd-input--mono"
                            name="editName"
                            [attr.aria-label]="'New name for ' + env.name"
                            [(ngModel)]="editName"
                            [disabled]="busy()"
                          />
                          <button
                            class="akd-btn akd-btn--secondary akd-btn--sm"
                            type="submit"
                            [disabled]="busy() || !editName.trim()"
                          >
                            Save
                          </button>
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            (click)="editing.set(null)"
                          >
                            Cancel
                          </button>
                        </form>
                      } @else {
                        <a
                          class="akd-mono"
                          [routerLink]="['/projects', uuid(), 'environments', env.uuid]"
                          (click)="$event.stopPropagation()"
                        >
                          {{ env.name }}
                        </a>
                      }
                    </td>
                    <td class="akd-muted">{{ env.description || '—' }}</td>
                    <td class="right">
                      <span class="akd-mono akd-muted">{{ env.resource_count ?? 0 }}</span>
                    </td>
                    <td class="right">
                      <div class="actions" (click)="$event.stopPropagation()">
                        <button
                          class="akd-iconbtn"
                          type="button"
                          [attr.aria-label]="'Rename environment ' + env.name"
                          [disabled]="busy()"
                          (click)="startRename(env)"
                        >
                          <akd-icon name="pencil" [size]="15" />
                        </button>
                        <button
                          class="akd-iconbtn"
                          type="button"
                          [attr.aria-label]="'Delete environment ' + env.name"
                          [disabled]="busy()"
                          (click)="removeEnvironment(env)"
                        >
                          <akd-icon name="trash-2" [size]="15" />
                        </button>
                        <span class="chevron" aria-hidden="true">
                          <akd-icon name="chevron-right" [size]="15" />
                        </span>
                      </div>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .crumbs {
        margin-bottom: var(--space-2);
      }
      .title {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        min-width: 0;
      }
      .akd-bar h1.title__name {
        font-family: var(--font-mono);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .section {
        display: block;
        margin-bottom: var(--space-5);
      }
      .row {
        display: flex;
        align-items: flex-end;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .row .grow {
        flex: 1;
        min-width: 14rem;
      }
      .envform {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .envform .akd-input {
        width: 14rem;
      }
      .rename {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .rename .akd-input {
        width: 11rem;
      }
      .actions {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
      }
      .chevron {
        color: var(--text-3);
        line-height: 0;
        margin-left: var(--space-1);
      }
    `,
  ],
})
export class ProjectDetailComponent {
  /** Bound from the route (`projects/:uuid`) by withComponentInputBinding. */
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly project = signal<Project | null>(null);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly editing = signal<string | null>(null);
  protected readonly crumbs = computed<Crumb[]>(() => [
    { label: 'Projects', link: '/projects' },
    { label: this.project()?.name ?? '…' },
  ]);

  protected name = '';
  protected description = '';
  protected envName = '';
  protected editName = '';

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const [project, envs] = await Promise.all([
        this.api.client().getProject(uuid),
        this.api.client().listEnvironments(uuid, { limit: 100 }),
      ]);
      this.project.set(project);
      this.environments.set(envs.data);
      this.name = project.name;
      this.description = project.description ?? '';
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async save(): Promise<void> {
    if (this.busy() || !this.name.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const updated = await this.api.client().updateProject(this.uuid(), {
        name: this.name.trim(),
        description: this.description.trim() || null,
      });
      this.project.set(updated);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    const p = this.project();
    if (!p) return;
    if (
      !confirm(
        `Delete the project "${p.name}"? Its environments are removed with it; this cannot be undone.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteProject(this.uuid());
      await this.router.navigateByUrl('/projects');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }

  protected async createEnvironment(): Promise<void> {
    if (this.busy() || !this.envName.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createEnvironment(this.uuid(), { name: this.envName.trim() });
      this.envName = '';
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openEnvironment(env: Environment): void {
    void this.router.navigate(['/projects', this.uuid(), 'environments', env.uuid]);
  }

  protected startRename(env: Environment): void {
    this.editing.set(env.uuid);
    this.editName = env.name;
  }

  protected async saveEnvironment(env: Environment): Promise<void> {
    if (this.busy() || !this.editName.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api
        .client()
        .updateEnvironment(this.uuid(), env.uuid, { name: this.editName.trim() });
      this.editing.set(null);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeEnvironment(env: Environment): Promise<void> {
    if (
      !confirm(
        `Delete the environment "${env.name}"? An environment still holding resources cannot be deleted.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteEnvironment(this.uuid(), env.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
