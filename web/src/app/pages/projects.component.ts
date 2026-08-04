import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];

@Component({
  selector: 'app-projects',
  standalone: true,
  imports: [FormsModule, RouterLink, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Projects</h1>
        @if (api.can('projects:manage')) {
          @if (creating()) {
            <button class="akd-btn akd-btn--ghost" type="button" (click)="creating.set(false)">
              Cancel
            </button>
          } @else {
            <button class="akd-btn akd-btn--primary" type="button" (click)="creating.set(true)">
              <akd-icon name="plus" [size]="15" />
              New project
            </button>
          }
        }
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <akd-card title="New project" class="create">
          <form class="create__form" (ngSubmit)="create()">
            <div class="akd-field">
              <label class="akd-field__label" for="pr-name">Name</label>
              <input
                id="pr-name"
                name="name"
                class="akd-input akd-input--mono"
                placeholder="my-product"
                required
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="pr-description">Description</label>
              <input
                id="pr-description"
                name="description"
                class="akd-input"
                [(ngModel)]="description"
                [disabled]="busy()"
              />
              <span class="akd-field__hint">Optional — shown on the project card.</span>
            </div>
            <div class="create__actions">
              <button
                class="akd-btn akd-btn--primary"
                type="submit"
                [disabled]="busy() || !name.trim()"
              >
                <akd-icon name="plus" [size]="15" />
                {{ busy() ? 'Creating…' : 'Create project' }}
              </button>
            </div>
          </form>
        </akd-card>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (projects().length === 0) {
        <akd-empty-state
          icon="folder-git-2"
          title="No projects yet"
          message="A project groups environments; environments hold the resources."
        >
          @if (api.can('projects:manage') && !creating()) {
            <button class="akd-btn akd-btn--primary" type="button" (click)="creating.set(true)">
              <akd-icon name="plus" [size]="15" />
              New project
            </button>
          }
        </akd-empty-state>
      } @else {
        <div class="grid">
          @for (project of projects(); track project.uuid) {
            <div class="pcard akd-card">
              <div class="pcard__head">
                <span class="pcard__icon"><akd-icon name="folder-git-2" [size]="17" /></span>
                <a class="pcard__name" [routerLink]="['/projects', project.uuid]">
                  {{ project.name }}
                </a>
                <span class="spacer"></span>
                @if (api.can('projects:manage')) {
                  <button
                    class="akd-iconbtn pcard__delete"
                    type="button"
                    [attr.aria-label]="'Delete project ' + project.name"
                    [disabled]="busy()"
                    (click)="remove(project)"
                  >
                    <akd-icon name="trash-2" [size]="15" />
                  </button>
                }
              </div>
              <div class="pcard__desc akd-muted">{{ project.description || '—' }}</div>
              <div class="pcard__badges">
                <span class="akd-badge akd-badge--mono">
                  {{ project.environments?.length ?? 0 }} environments
                </span>
              </div>
            </div>
          }
        </div>
      }
    </div>
  `,
  styles: [
    `
      .create {
        display: block;
        margin-bottom: var(--space-5);
        max-width: 32rem;
      }
      .create__form {
        display: grid;
        gap: var(--space-4);
      }
      .create__actions {
        display: flex;
        justify-content: flex-end;
      }
      .grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(21rem, 1fr));
        gap: var(--space-4);
      }
      .pcard {
        position: relative;
        display: grid;
        gap: var(--space-3);
        transition: border-color var(--dur-1) var(--ease-out);
      }
      .pcard:hover {
        border-color: var(--border-2);
      }
      .pcard__head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        min-width: 0;
      }
      .pcard__icon {
        color: var(--accent);
        display: inline-flex;
      }
      .pcard__name {
        font: var(--weight-semibold) var(--text-lg) var(--font-mono);
        color: var(--text-1);
        text-decoration: none;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      /* Stretched link: the whole card navigates to the project. */
      .pcard__name::after {
        content: '';
        position: absolute;
        inset: 0;
      }
      .spacer {
        flex: 1;
      }
      .pcard__delete {
        position: relative;
        z-index: 1;
      }
      .pcard__desc {
        font-size: var(--text-sm);
      }
      .pcard__badges {
        display: flex;
        gap: var(--space-2);
      }
    `,
  ],
})
export class ProjectsComponent {
  protected readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly projects = signal<Project[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected name = '';
  protected description = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const projects = await fetchAll((cursor) =>
        this.api.client().listProjects({ limit: 100, cursor }),
      );
      this.projects.set(projects);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.name.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createProject({
        name: this.name.trim(),
        description: this.description.trim() || null,
      });
      this.name = '';
      this.description = '';
      this.creating.set(false);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(project: Project): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the project',
        message: `Delete the project "${project.name}"? Its environments are removed with it; environments still holding resources block the deletion.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteProject(project.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
