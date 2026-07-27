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
import { Router } from '@angular/router';
import { SlicePipe } from '@angular/common';
import { StatusBadgeComponent } from '../../../ui/status-badge/status-badge.component';
import { CardComponent } from '../../../ui/card/card.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type Deployment = components['schemas']['Deployment'];

const TERMINAL: Deployment['status'][] = ['succeeded', 'failed', 'cancelled', 'superseded'];

@Component({
  selector: 'app-application-deployments-tab',
  standalone: true,
  imports: [StatusBadgeComponent, CardComponent, EmptyStateComponent, IconComponent, SlicePipe],
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
      <akd-empty-state
        icon="rocket"
        title="No deployment yet"
        message="Trigger one with the Deploy button above — it appears here with its status and logs."
      />
    } @else {
      <akd-card title="History" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Deployment history of this application
          </caption>
          <thead>
            <tr>
              <th scope="col">Commit</th>
              <th scope="col">Status</th>
              <th scope="col">Trigger</th>
              <th scope="col">Duration</th>
              <th scope="col">Created</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (d of deployments(); track d.uuid) {
              <tr
                class="clickable"
                tabindex="0"
                role="link"
                [attr.aria-label]="'Open build logs of the deployment created ' + d.created_at"
                (click)="open(d)"
                (keydown.enter)="open(d)"
              >
                <td>
                  @if (d.commit_sha) {
                    <span class="akd-badge akd-badge--mono">{{ d.commit_sha | slice: 0 : 8 }}</span>
                    @if (d.commit_message) {
                      <span class="akd-muted commit-msg"> {{ d.commit_message }}</span>
                    }
                    @if (d.commit_author) {
                      <span class="akd-muted commit-author"> · {{ d.commit_author }}</span>
                    }
                  } @else {
                    <span class="akd-muted">—</span>
                  }
                </td>
                <td><akd-status-badge domain="deployment" [state]="d.status" /></td>
                <td class="akd-muted">{{ d.trigger }}{{ d.pr_id ? ' #' + d.pr_id : '' }}{{ d.is_rollback ? ' · rollback' : '' }}</td>
                <td>
                  <span class="akd-mono akd-muted">{{ duration(d) }}</span>
                </td>
                <td class="akd-muted">{{ d.created_at }}</td>
                <td class="right">
                  <!-- Actions must not trigger the row's navigation. -->
                  <div class="row-actions" (click)="$event.stopPropagation()">
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      (click)="open(d)"
                      aria-label="View build logs"
                    >
                      <akd-icon name="scroll-text" [size]="13" />
                      Logs
                    </button>
                    @if (cancellable(d)) {
                      <button
                        class="akd-btn akd-btn--danger akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="cancel(d)"
                      >
                        Cancel
                      </button>
                    }
                    @if (d.status === 'succeeded' && !d.is_rollback) {
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="rollback(d)"
                      >
                        <akd-icon name="rotate-ccw" [size]="13" />
                        Roll back to this
                      </button>
                    }
                  </div>
                </td>
              </tr>
            }
          </tbody>
        </table>
        @if (hasMore()) {
          <div class="load-more">
            <button
              class="akd-btn akd-btn--ghost akd-btn--sm"
              type="button"
              [disabled]="loadingMore()"
              (click)="loadMore()"
            >
              {{ loadingMore() ? 'Loading…' : 'Load more' }}
            </button>
          </div>
        }
      </akd-card>
    }
  `,
  styles: [
    `
      .row-actions {
        display: flex;
        gap: var(--space-2);
        justify-content: flex-end;
      }
      .commit-msg {
        font-size: var(--text-sm);
      }
      .load-more {
        display: flex;
        justify-content: center;
        padding: var(--space-3);
        border-top: 1px solid var(--border-1);
      }
      .clickable {
        cursor: pointer;
      }
      .clickable:hover {
        background: var(--bg-2);
      }
      .clickable:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
    `,
  ],
})
export class ApplicationDeploymentsTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly loading = signal(true);
  protected readonly loadingMore = signal(false);
  /** Opaque cursor of the next page; null once the history is exhausted. */
  private readonly cursor = signal<string | null>(null);
  protected readonly hasMore = computed(() => this.cursor() !== null);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  /** Open the deployment's own page (its build logs live there now). */
  protected open(d: Deployment): void {
    void this.router.navigate(['/applications', this.uuid(), 'deployments', d.uuid]);
  }

  protected cancellable(d: Deployment): boolean {
    return !TERMINAL.includes(d.status);
  }

  /** Derived from started_at/finished_at — a deployment still running has none. */
  protected duration(d: Deployment): string {
    if (!d.started_at || !d.finished_at) return '—';
    const ms = Date.parse(d.finished_at) - Date.parse(d.started_at);
    if (!Number.isFinite(ms) || ms < 0) return '—';
    const seconds = Math.round(ms / 1000);
    return seconds >= 60 ? `${Math.floor(seconds / 60)}m ${seconds % 60}s` : `${seconds}s`;
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const page = await this.api.client().listApplicationDeployments(uuid, { limit: 50 });
      this.deployments.set(page.data);
      this.cursor.set(page.next_cursor ?? null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  /** Append the next page, following the cursor until the history is exhausted. */
  protected async loadMore(): Promise<void> {
    const cursor = this.cursor();
    if (cursor === null || this.loadingMore()) return;
    this.loadingMore.set(true);
    try {
      const page = await this.api
        .client()
        .listApplicationDeployments(this.uuid(), { limit: 50, cursor });
      this.deployments.update((rows) => [...rows, ...page.data]);
      this.cursor.set(page.next_cursor ?? null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loadingMore.set(false);
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
