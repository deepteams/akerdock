import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { SlicePipe } from '@angular/common';
import { StatusBadgeComponent } from '../../../ui/status-badge/status-badge.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type Deployment = components['schemas']['Deployment'];

const TERMINAL: Deployment['status'][] = ['succeeded', 'failed', 'cancelled', 'superseded'];

@Component({
  selector: 'app-application-deployments-tab',
  standalone: true,
  imports: [StatusBadgeComponent, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }
    @if (notice(); as message) {
      <p class="akd-muted" role="status">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (deployments().length === 0) {
      <div class="akd-empty">
        <p><strong>No deployment yet.</strong></p>
      </div>
    } @else {
      <table class="akd-table">
        <caption class="sr-only">Deployment history of this application</caption>
        <thead>
          <tr>
            <th scope="col">Status</th>
            <th scope="col">Trigger</th>
            <th scope="col">Commit</th>
            <th scope="col">Created</th>
            <th scope="col"><span class="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          @for (d of deployments(); track d.uuid) {
            <tr>
              <td><akd-status-badge domain="deployment" [state]="d.status" /></td>
              <td class="akd-muted">
                {{ d.trigger }}{{ d.is_rollback ? ' · rollback' : '' }}
              </td>
              <td>
                @if (d.commit_sha) {
                  <span class="akd-mono">{{ d.commit_sha | slice: 0 : 8 }}</span>
                  @if (d.commit_message) {
                    <span class="akd-muted"> {{ d.commit_message }}</span>
                  }
                } @else {
                  <span class="akd-muted">—</span>
                }
              </td>
              <td class="akd-muted">{{ d.created_at }}</td>
              <td class="right">
                @if (cancellable(d)) {
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="cancel(d)"
                  >
                    Cancel
                  </button>
                }
                @if (d.status === 'succeeded' && !d.is_rollback) {
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="rollback(d)"
                  >
                    Roll back to this
                  </button>
                }
              </td>
            </tr>
          }
        </tbody>
      </table>
    }
  `,
})
export class ApplicationDeploymentsTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected cancellable(d: Deployment): boolean {
    return !TERMINAL.includes(d.status);
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const page = await this.api.client().listApplicationDeployments(uuid, { limit: 50 });
      this.deployments.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async cancel(d: Deployment): Promise<void> {
    if (!confirm('Cancel this deployment? The currently routed container stays untouched.')) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().cancelDeployment(d.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * A rollback is a new deployment of a past image (202): it is queued and
   * observed like any other — it never rewrites history.
   */
  protected async rollback(d: Deployment): Promise<void> {
    if (
      !confirm(
        'Roll back to the image of this deployment? A new rollback deployment is queued and replaces what is currently running.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      await this.api.client().rollbackApplication(this.uuid(), { deployment_uuid: d.uuid });
      this.notice.set('Rollback queued — it appears below as a new deployment.');
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
