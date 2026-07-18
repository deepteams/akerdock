import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Application = components['schemas']['Application'];

@Component({
  selector: 'app-applications',
  standalone: true,
  imports: [RouterLink, CardComponent, EmptyStateComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Applications</h1>
        <a class="akd-btn akd-btn--primary" routerLink="/applications/new">
          <akd-icon name="plus" [size]="15" />
          New application
        </a>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (applications().length === 0) {
        <akd-empty-state
          icon="rocket"
          title="No applications yet"
          message="Deploy from a Docker image, an inline Dockerfile, or a Git repository."
        >
          <a class="akd-btn akd-btn--primary" routerLink="/applications/new">
            <akd-icon name="plus" [size]="15" />
            Create your first application
          </a>
        </akd-empty-state>
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Applications of this team, with their desired and observed state
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Source</th>
                <th scope="col">Desired</th>
                <th scope="col">Observed</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (app of applications(); track app.uuid) {
                <tr>
                  <td>
                    <span class="name-cell">
                      <akd-icon name="rocket" [size]="15" />
                      <a class="akd-mono" [routerLink]="['/applications', app.uuid]">{{
                        app.name
                      }}</a>
                    </span>
                  </td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ app.source_type }}</span>
                  </td>
                  <td>
                    <!-- The desired state is an intent, not an observation: it uses the
                         resource machine but must never be confused with what is
                         actually running (§21.2). -->
                    <akd-status-badge domain="resource" [state]="app.desired_status" />
                  </td>
                  <td>
                    <akd-status-badge domain="resource" [state]="app.observed_status" />
                  </td>
                  <td class="right">
                    <button
                      class="akd-btn akd-btn--secondary akd-btn--sm"
                      type="button"
                      [disabled]="deploying() === app.uuid"
                      (click)="deploy(app)"
                    >
                      <akd-icon name="rocket" [size]="13" />
                      {{ deploying() === app.uuid ? 'Queued…' : 'Deploy' }}
                    </button>
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
      .name-cell {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
        color: var(--text-3);
      }
    `,
  ],
})
export class ApplicationsComponent {
  private readonly api = inject(ApiService);

  protected readonly applications = signal<Application[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly deploying = signal<string | null>(null);

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    const client = this.api.client();
    try {
      const page = await client.listApplications({ limit: 100 });
      this.applications.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  /**
   * A deploy is a long action, so it becomes a JOB (§4.2): the UI does not wait
   * for it, it observes it. Closing the page never cancels the deployment — the
   * control plane executes, the UI watches.
   */
  protected async deploy(app: Application): Promise<void> {
    const client = this.api.client();
    if (!app.uuid) return;
    this.deploying.set(app.uuid);
    this.error.set(null);
    try {
      await client.deployApplication(app.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.deploying.set(null);
    }
  }
}
