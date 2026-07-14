import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];

@Component({
  selector: 'app-project-detail',
  standalone: true,
  imports: [FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div>
          <a routerLink="/projects" class="back">← Projects</a>
          <h1>{{ project()?.name ?? '…' }}</h1>
        </div>
        <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove()">
          Delete project
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (project(); as p) {
        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Details</h2>
          </header>
          <form class="row" (ngSubmit)="save()">
            <div class="akd-field grow">
              <label for="pd-name">Name</label>
              <input
                id="pd-name"
                name="name"
                class="akd-input"
                required
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field grow">
              <label for="pd-description">Description</label>
              <input
                id="pd-description"
                name="description"
                class="akd-input"
                [(ngModel)]="description"
                [disabled]="busy()"
              />
            </div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !name.trim()">Save</button>
          </form>
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Environments</h2>
          </header>

          <form class="row" (ngSubmit)="createEnvironment()">
            <div class="akd-field grow">
              <label for="env-name">New environment</label>
              <input
                id="env-name"
                name="envName"
                class="akd-input"
                placeholder="e.g. production, staging"
                [(ngModel)]="envName"
                [disabled]="busy()"
              />
            </div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !envName.trim()">
              Add
            </button>
          </form>

          @if (environments().length === 0) {
            <div class="akd-empty">
              <p><strong>No environments yet.</strong></p>
              <p>Resources live inside an environment — create one to start deploying.</p>
            </div>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">Environments of this project</caption>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Description</th>
                  <th scope="col">Resources</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (env of environments(); track env.uuid) {
                  <tr>
                    <td>
                      @if (editing() === env.uuid) {
                        <form class="row" (ngSubmit)="saveEnvironment(env)">
                          <input
                            class="akd-input"
                            name="editName"
                            [attr.aria-label]="'New name for ' + env.name"
                            [(ngModel)]="editName"
                            [disabled]="busy()"
                          />
                          <button class="akd-btn" type="submit" [disabled]="busy() || !editName.trim()">
                            Save
                          </button>
                          <button class="akd-btn-ghost" type="button" (click)="editing.set(null)">
                            Cancel
                          </button>
                        </form>
                      } @else {
                        {{ env.name }}
                      }
                    </td>
                    <td class="akd-muted">{{ env.description || '—' }}</td>
                    <td class="akd-muted">{{ env.resource_count ?? 0 }}</td>
                    <td class="right">
                      <button
                        class="akd-btn-ghost"
                        type="button"
                        [disabled]="busy()"
                        (click)="startRename(env)"
                      >
                        Rename
                      </button>
                      <button
                        class="akd-btn-danger"
                        type="button"
                        [disabled]="busy()"
                        (click)="removeEnvironment(env)"
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </section>
      }
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
      .akd-bar h1 {
        margin-top: var(--akd-space-1);
      }
      .section {
        margin-bottom: var(--akd-space-5);
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--akd-space-2);
        flex-wrap: wrap;
      }
      .row .grow {
        flex: 1;
        min-width: 14rem;
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
