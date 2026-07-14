import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];

@Component({
  selector: 'app-projects',
  standalone: true,
  imports: [FormsModule, RouterLink, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Projects</h1>
        <button class="akd-btn" type="button" (click)="creating.set(!creating())">
          {{ creating() ? 'Cancel' : 'New project' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label for="pr-name">Name</label>
            <input
              id="pr-name"
              name="name"
              class="akd-input"
              required
              [(ngModel)]="name"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label for="pr-description">Description (optional)</label>
            <input
              id="pr-description"
              name="description"
              class="akd-input"
              [(ngModel)]="description"
              [disabled]="busy()"
            />
          </div>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !name.trim()">
              {{ busy() ? 'Creating…' : 'Create project' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (projects().length === 0) {
        <div class="akd-empty">
          <p><strong>No projects yet.</strong></p>
          <p>A project groups environments; environments hold the resources.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Projects of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Description</th>
              <th scope="col">Environments</th>
              <th scope="col">Created</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (project of projects(); track project.uuid) {
              <tr>
                <td>
                  <a [routerLink]="['/projects', project.uuid]">{{ project.name }}</a>
                </td>
                <td class="akd-muted">{{ project.description || '—' }}</td>
                <td class="akd-muted">{{ project.environments?.length ?? 0 }}</td>
                <td class="akd-muted">{{ project.created_at | slice: 0 : 10 }}</td>
                <td class="right">
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(project)"
                  >
                    Delete
                  </button>
                </td>
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
        max-width: 32rem;
      }
    `,
  ],
})
export class ProjectsComponent {
  private readonly api = inject(ApiService);

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
      const page = await this.api.client().listProjects({ limit: 100 });
      this.projects.set(page.data);
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
      !confirm(
        `Delete the project "${project.name}"? Its environments are removed with it; environments still holding resources block the deletion.`,
      )
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
