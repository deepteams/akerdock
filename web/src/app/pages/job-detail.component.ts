import { SlicePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiError } from '../../api/client';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Job = components['schemas']['Job'];
type ErrorDetail = components['schemas']['ErrorDetail'];

@Component({
  selector: 'app-job-detail',
  standalone: true,
  imports: [RouterLink, SlicePipe, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <p><a routerLink="/jobs" class="akd-muted">← Jobs</a></p>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (job(); as j) {
        <header class="akd-bar">
          <h1 class="akd-mono">{{ j.type }}</h1>
          @if (j.status === 'dead_letter') {
            <div class="head-actions">
              <button class="akd-btn" type="button" [disabled]="busy()" (click)="retry(j)">
                Retry
              </button>
              <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="forget(j)">
                Forget
              </button>
            </div>
          }
        </header>

        @if (retriedAs(); as newUuid) {
          <p class="akd-muted" role="status">
            Retry queued as <a [routerLink]="['/jobs', newUuid]" class="akd-mono">{{ newUuid }}</a> —
            this job stays as the record of the failed attempt.
          </p>
        }

        <dl class="akd-dl">
          <dt>Status</dt>
          <dd><akd-status-badge domain="job" [state]="j.status" /></dd>
          <dt>Queue</dt>
          <dd>{{ j.queue ?? '—' }}</dd>
          <dt>Attempt</dt>
          <dd>{{ j.attempt }}</dd>
          @if (j.resource_type) {
            <dt>Resource</dt>
            <dd class="akd-mono">{{ j.resource_type }} {{ j.resource_uuid }}</dd>
          }
          @if (j.retry_of_uuid; as origin) {
            <dt>Retry of</dt>
            <dd><a [routerLink]="['/jobs', origin]" class="akd-mono">{{ origin }}</a></dd>
          }
          <dt>Created</dt>
          <dd>{{ j.created_at | slice: 0 : 19 }}</dd>
          @if (j.finished_at; as finished) {
            <dt>Finished</dt>
            <dd>{{ finished | slice: 0 : 19 }}</dd>
          }
          @if (j.dead_lettered_at; as dead) {
            <dt>Dead-lettered</dt>
            <dd>{{ dead | slice: 0 : 19 }}</dd>
          }
        </dl>

        @if (remnants(); as items) {
          <section class="akd-card remnants">
            <h2>Remnants on the server</h2>
            <p class="akd-muted">
              This job left objects behind — forgetting it deletes nothing remotely. Clean them up
              by hand, or acknowledge them to close the job anyway (the acknowledgement is
              audited).
            </p>
            <ul class="akd-mono">
              @for (item of items; track $index) {
                <li>{{ item.field ? item.field + ': ' : '' }}{{ item.message }}</li>
              }
            </ul>
            <div>
              <button
                class="akd-btn-danger"
                type="button"
                [disabled]="busy()"
                (click)="forgetAnyway(j)"
              >
                Forget anyway (acknowledge remnants)
              </button>
            </div>
          </section>
        }

        @if (j.steps?.length) {
          <section class="akd-card">
            <h2>Steps</h2>
            <table class="akd-table">
              <caption class="sr-only">Steps of this job</caption>
              <thead>
                <tr>
                  <th scope="col">Step</th>
                  <th scope="col">Status</th>
                  <th scope="col">Detail</th>
                </tr>
              </thead>
              <tbody>
                @for (step of j.steps; track $index) {
                  <tr>
                    <td class="akd-mono">{{ step.name }}</td>
                    <td><akd-status-badge domain="task" [state]="step.status" /></td>
                    <td class="akd-muted">{{ step.message ?? '—' }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </section>
        }

        @if (j.result) {
          <section class="akd-card">
            <h2>Result</h2>
            <pre class="akd-mono block">{{ json(j.result) }}</pre>
          </section>
        }

        @if (j.error; as jobError) {
          <section class="akd-card">
            <h2>Error</h2>
            <p>{{ jobError.message }} <span class="akd-muted">({{ jobError.code }})</span></p>
            @if (jobError.details.length) {
              <pre class="akd-mono block">{{ json(jobError.details) }}</pre>
            }
          </section>
        }
      }
    </div>
  `,
  styles: [
    `
      .head-actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .akd-dl {
        margin-bottom: var(--akd-space-5);
      }
      .akd-card {
        margin-bottom: var(--akd-space-5);
      }
      .block {
        margin: 0;
        padding: var(--akd-space-3);
        background: var(--akd-bg);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-sm);
        overflow-x: auto;
      }
      .remnants ul {
        margin: 0;
        padding-left: var(--akd-space-5);
      }
    `,
  ],
})
export class JobDetailComponent {
  private readonly api = inject(ApiService);

  /** Bound by the router from the :uuid path parameter. */
  readonly uuid = input.required<string>();

  protected readonly job = signal<Job | null>(null);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly retriedAs = signal<string | null>(null);
  /** What the failed job left behind on the server, per the 409 details. */
  protected readonly remnants = signal<ErrorDetail[] | null>(null);

  constructor() {
    // Router inputs are not set at construction time; the effect fires once
    // they are, and again if the route parameter changes in place.
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      this.job.set(await this.api.client().getJob(uuid));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected json(value: unknown): string {
    return JSON.stringify(value, null, 2);
  }

  protected async retry(job: Job): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      // 202: a NEW linked job is created — this one keeps its dead-letter
      // history. Point the operator at the new attempt, don't poll.
      const accepted = await this.api.client().retryJob(job.uuid);
      this.retriedAs.set(accepted.job_uuid);
      await this.load(job.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async forget(job: Job): Promise<void> {
    if (!confirm('Forget this job? It is closed as cancelled and leaves the dead-letter list.')) {
      return;
    }
    await this.doForget(job, false);
  }

  protected async forgetAnyway(job: Job): Promise<void> {
    await this.doForget(job, true);
  }

  private async doForget(job: Job, acknowledgeRemnants: boolean): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api
        .client()
        .forgetJob(job.uuid, acknowledgeRemnants ? { acknowledge_remnants: true } : undefined);
      this.remnants.set(null);
      await this.load(job.uuid);
    } catch (err) {
      // 409 remnants_present: the job left real objects on the server, and the
      // error's details name them. Show the list and offer the acknowledged
      // forget — which is audited and deletes nothing remotely.
      if (err instanceof ApiError && err.hasRemnants) {
        this.remnants.set(err.details);
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }
}
