import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Application = components['schemas']['Application'];

@Component({
  selector: 'app-applications',
  standalone: true,
  imports: [StatusBadgeComponent, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <header class="bar">
      <h1>Applications</h1>
      <a class="akd-btn new" routerLink="/applications/new">New application</a>
    </header>

    @if (error(); as message) {
      <p class="error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="muted">Loading…</p>
    } @else if (applications().length === 0) {
      <div class="empty">
        <p><strong>No applications yet.</strong></p>
        <p class="muted">
          <a routerLink="/applications/new">Create your first application</a> — from a Docker
          image, an inline Dockerfile, or a Git repository.
        </p>
      </div>
    } @else {
      <table>
        <caption class="sr-only">
          Applications of this team, with their desired and observed state
        </caption>
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Source</th>
            <th scope="col">Desired</th>
            <th scope="col">Observed</th>
            <th scope="col"><span class="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          @for (app of applications(); track app.uuid) {
            <tr>
              <td class="name">
                <a [routerLink]="['/applications', app.uuid]">{{ app.name }}</a>
              </td>
              <td class="muted">{{ app.source_type }}</td>
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
                  class="ghost"
                  type="button"
                  [disabled]="deploying() === app.uuid"
                  (click)="deploy(app)"
                >
                  {{ deploying() === app.uuid ? 'Queued…' : 'Deploy' }}
                </button>
              </td>
            </tr>
          }
        </tbody>
      </table>
    }
  `,
  styles: [
    `
      :host {
        display: block;
        padding: var(--akd-space-6);
        background: var(--akd-bg);
        min-height: 100vh;
      }
      .bar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        margin-bottom: var(--akd-space-5);
      }
      .new {
        text-decoration: none;
      }
      h1 {
        margin: 0;
        font-size: var(--akd-text-xl);
        color: var(--akd-text);
      }
      table {
        width: 100%;
        border-collapse: collapse;
        font-size: var(--akd-text-sm);
      }
      th,
      td {
        padding: var(--akd-space-2) var(--akd-space-3);
        text-align: left;
        border-bottom: 1px solid var(--akd-border);
      }
      th {
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.04em;
      }
      td {
        color: var(--akd-text);
      }
      td.right {
        text-align: right;
      }
      .name {
        font-weight: var(--akd-weight-medium);
      }
      .name a {
        color: var(--akd-text);
        text-decoration: none;
      }
      .name a:hover {
        text-decoration: underline;
      }
      .name a:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: 2px;
      }
      .muted {
        color: var(--akd-text-secondary);
      }
      .ghost {
        padding: var(--akd-space-1) var(--akd-space-3);
        font: inherit;
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
        background: transparent;
        border: 1px solid var(--akd-border-input);
        border-radius: var(--akd-radius-sm);
        cursor: pointer;
      }
      .ghost:hover:not(:disabled) {
        background: var(--akd-surface-hover);
      }
      .ghost:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      .ghost:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: 1px;
      }
      .empty {
        padding: var(--akd-space-8);
        text-align: center;
        border: 1px dashed var(--akd-border);
        border-radius: var(--akd-radius-lg);
      }
      .error {
        padding: var(--akd-space-2) var(--akd-space-3);
        color: var(--akd-status-danger-fg);
        background: var(--akd-status-danger-bg);
        border-radius: var(--akd-radius-sm);
      }
      .sr-only {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
        border: 0;
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
