import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
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

@Component({
  selector: 'app-jobs',
  standalone: true,
  imports: [FormsModule, RouterLink, SlicePipe, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Jobs</h1>
        <div class="filters">
          <label class="sr-only" for="job-status">Filter by status</label>
          <select
            id="job-status"
            name="status"
            class="akd-select"
            [(ngModel)]="status"
            (ngModelChange)="reload()"
          >
            <option value="">all statuses</option>
            @for (s of statuses; track s) {
              <option [value]="s">{{ s }}</option>
            }
          </select>
          <label class="sr-only" for="job-queue">Filter by queue</label>
          <select
            id="job-queue"
            name="queue"
            class="akd-select"
            [(ngModel)]="queue"
            (ngModelChange)="reload()"
          >
            <option value="">all queues</option>
            @for (q of queues; track q) {
              <option [value]="q">{{ q }}</option>
            }
          </select>
        </div>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (jobs().length === 0) {
        <div class="akd-empty">
          <p><strong>No jobs match.</strong></p>
          <p>Long-running operations show up here as they are queued.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Asynchronous jobs of this team</caption>
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
                  <a [routerLink]="['/jobs', job.uuid]" class="akd-mono">{{ job.type }}</a>
                </td>
                <td class="akd-muted">{{ job.queue ?? '—' }}</td>
                <td><akd-status-badge domain="job" [state]="job.status" /></td>
                <td class="akd-muted">{{ job.attempt }}</td>
                <td class="akd-muted">{{ job.created_at | slice: 0 : 19 }}</td>
              </tr>
            }
          </tbody>
        </table>
      }
    </div>
  `,
  styles: [
    `
      .filters {
        display: flex;
        gap: var(--akd-space-2);
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
  protected readonly error = signal<string | null>(null);

  protected status: JobStatus | '' = '';
  protected queue = '';

  constructor() {
    void this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      const page = await this.api.client().listJobs({
        limit: 100,
        status: this.status || undefined,
        queue: this.queue || undefined,
      });
      this.jobs.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }
}
