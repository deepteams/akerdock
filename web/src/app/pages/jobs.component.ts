import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatComponent } from '../../ui/stat/stat.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Job = components['schemas']['Job'];
type JobStatus = components['schemas']['JobStatus'];

const STATUSES: JobStatus[] = [
  'scheduled',
  'queued',
  'leased',
  'running',
  'retry_wait',
  'succeeded',
  'cancelled',
  'dead_letter',
];

/** Logical queues of §24.3 — the filter offers them, the API takes any string. */
const QUEUES = ['deploy', 'backup', 'cleanup', 'maintenance'];

/** Icon per job type family — the segment before the first dot. */
const TYPE_ICONS: Record<string, string> = {
  server: 'server',
  application: 'rocket',
  database: 'database',
  backup: 'archive',
  certificate: 'shield-check',
  encryption: 'key-round',
};

@Component({
  selector: 'app-jobs',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    SlicePipe,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
    StatComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Jobs</h1>
        <div class="filters">
          @if (type) {
            <span class="akd-badge akd-badge--accent akd-badge--mono">
              type: {{ type }}
              <button
                class="chip-clear"
                type="button"
                aria-label="Clear type filter"
                (click)="clearType()"
              >
                <akd-icon name="x" [size]="12" />
              </button>
            </span>
          }
          <label class="sr-only" for="job-status">Filter by status</label>
          <div class="akd-select">
            <select
              id="job-status"
              name="status"
              class="akd-input"
              [(ngModel)]="status"
              (ngModelChange)="reload()"
            >
              <option value="">all statuses</option>
              @for (s of statuses; track s) {
                <option [value]="s">{{ s }}</option>
              }
            </select>
          </div>
          <label class="sr-only" for="job-queue">Filter by queue</label>
          <div class="akd-select">
            <select
              id="job-queue"
              name="queue"
              class="akd-input"
              [(ngModel)]="queue"
              (ngModelChange)="reload()"
            >
              <option value="">all queues</option>
              @for (q of queues; track q) {
                <option [value]="q">{{ q }}</option>
              }
            </select>
          </div>
        </div>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else {
        <div class="stats">
          <akd-card><akd-stat label="Jobs" [value]="stats().total" /></akd-card>
          <akd-card><akd-stat label="Running" [value]="stats().running" /></akd-card>
          <akd-card><akd-stat label="Succeeded" [value]="stats().succeeded" /></akd-card>
          <akd-card><akd-stat label="Dead-letter" [value]="stats().deadLetter" /></akd-card>
        </div>

        @if (jobs().length === 0) {
          <akd-empty-state
            icon="clock"
            title="No jobs match."
            message="Long-running operations show up here as they are queued."
          />
        } @else {
          <akd-card [padded]="false">
            <table class="akd-table">
              <caption class="sr-only">
                Asynchronous jobs of this team
              </caption>
              <thead>
                <tr>
                  <th scope="col">Type</th>
                  <th scope="col">Queue</th>
                  <th scope="col">Status</th>
                  <th scope="col">Attempt</th>
                  <th scope="col">Created</th>
                </tr>
              </thead>
              <tbody>
                @for (job of jobs(); track job.uuid) {
                  <tr>
                    <td>
                      <a [routerLink]="['/jobs', job.uuid]" class="type-link">
                        <akd-icon [name]="icon(job.type)" [size]="15" />
                        <span class="akd-mono">{{ job.type }}</span>
                      </a>
                    </td>
                    <td>
                      @if (job.queue; as q) {
                        <span class="akd-badge akd-badge--mono">{{ q }}</span>
                      } @else {
                        <span class="akd-muted">—</span>
                      }
                    </td>
                    <td><akd-status-badge domain="job" [state]="job.status" /></td>
                    <td class="akd-muted">{{ job.attempt }}</td>
                    <td class="akd-mono date">{{ job.created_at | slice: 0 : 19 }}</td>
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
      }
    </div>
  `,
  styles: [
    `
      .filters {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .load-more {
        display: flex;
        justify-content: center;
        padding: var(--space-3);
        border-top: 1px solid var(--border-1);
      }
      .chip-clear {
        all: unset;
        cursor: pointer;
        display: inline-flex;
        line-height: 0;
        margin-left: 2px;
      }
      .chip-clear:focus-visible {
        outline: 2px solid var(--accent);
        outline-offset: 1px;
        border-radius: var(--radius-1);
      }
      .stats {
        display: grid;
        grid-template-columns: repeat(4, minmax(0, 1fr));
        gap: var(--space-4);
        margin-bottom: var(--space-5);
      }
      .type-link {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
      }
      .type-link akd-icon {
        color: var(--text-3);
      }
      .date {
        color: var(--text-3);
        white-space: nowrap;
      }
    `,
  ],
})
export class JobsComponent {
  private readonly api = inject(ApiService);

  protected readonly statuses = STATUSES;
  protected readonly queues = QUEUES;
  protected readonly jobs = signal<Job[]>([]);
  protected readonly loading = signal(true);
  protected readonly loadingMore = signal(false);
  /** Opaque cursor of the next page; null once the list is exhausted. */
  private readonly cursor = signal<string | null>(null);
  protected readonly hasMore = computed(() => this.cursor() !== null);
  protected readonly error = signal<string | null>(null);

  /** Counts derived from the loaded page only — no extra API call. */
  protected readonly stats = computed(() => {
    const jobs = this.jobs();
    const count = (status: JobStatus) => jobs.filter((job) => job.status === status).length;
    return {
      total: jobs.length,
      running: count('running'),
      succeeded: count('succeeded'),
      deadLetter: count('dead_letter'),
    };
  });

  protected status: JobStatus | '' = '';
  protected queue = '';
  /** Free-form type filter — set by deep links (?type=server.validate). */
  protected type = '';

  constructor() {
    this.type = inject(ActivatedRoute).snapshot.queryParamMap.get('type') ?? '';
    void this.reload();
  }

  protected clearType(): void {
    this.type = '';
    void this.reload();
  }

  protected icon(type: string): string {
    if (type.startsWith('backup.restore')) return 'archive-restore';
    return TYPE_ICONS[type.split('.')[0]] ?? 'clock';
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      const page = await this.api.client().listJobs({
        limit: 100,
        status: this.status || undefined,
        queue: this.queue || undefined,
        type: this.type || undefined,
      });
      this.jobs.set(page.data);
      this.cursor.set(page.next_cursor ?? null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  /** Append the next page, following the cursor until the list is exhausted. */
  protected async loadMore(): Promise<void> {
    const cursor = this.cursor();
    if (cursor === null || this.loadingMore()) return;
    this.loadingMore.set(true);
    try {
      const page = await this.api.client().listJobs({
        limit: 100,
        cursor,
        status: this.status || undefined,
        queue: this.queue || undefined,
        type: this.type || undefined,
      });
      this.jobs.update((rows) => [...rows, ...page.data]);
      this.cursor.set(page.next_cursor ?? null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loadingMore.set(false);
    }
  }
}
