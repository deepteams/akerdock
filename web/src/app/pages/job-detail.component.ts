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
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiError } from '../../api/client';
import { ApiService } from '../core/api.service';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import type { components } from '../../api/schema';

type Job = components['schemas']['Job'];
type ErrorDetail = components['schemas']['ErrorDetail'];

@Component({
  selector: 'app-job-detail',
  standalone: true,
  imports: [RouterLink, SlicePipe, CardComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (job(); as j) {
        <header class="akd-bar">
          <div class="head">
            <a
              routerLink="/jobs"
              class="akd-iconbtn akd-iconbtn--bordered"
              aria-label="Back to jobs"
            >
              <akd-icon name="arrow-left" [size]="16" />
            </a>
            <h1 class="akd-mono">{{ j.type }}</h1>
            <akd-status-badge domain="job" [state]="j.status" />
          </div>
          @if (j.status === 'dead_letter') {
            <div class="head-actions">
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                [disabled]="busy()"
                (click)="retry(j)"
              >
                <akd-icon name="rotate-ccw" [size]="14" />
                Retry
              </button>
              <button
                class="akd-btn akd-btn--danger akd-btn--sm"
                type="button"
                [disabled]="busy()"
                (click)="forget(j)"
              >
                Forget
              </button>
            </div>
          }
        </header>

        @if (retriedAs(); as newUuid) {
          <p class="akd-muted" role="status">
            Retry queued as
            <a [routerLink]="['/jobs', newUuid]" class="akd-mono">{{ newUuid }}</a> — this job stays
            as the record of the failed attempt.
          </p>
        }

        <div class="stack">
          <akd-card title="Details">
            <dl class="akd-dl">
              <dt>Queue</dt>
              <dd>
                @if (j.queue; as queue) {
                  <span class="akd-badge akd-badge--mono">{{ queue }}</span>
                } @else {
                  —
                }
              </dd>
              <dt>Attempt</dt>
              <dd>{{ j.attempt }}</dd>
              @if (j.resource_type) {
                <dt>Resource</dt>
                <dd class="akd-mono">{{ j.resource_type }} {{ j.resource_uuid }}</dd>
              }
              @if (j.retry_of_uuid; as origin) {
                <dt>Retry of</dt>
                <dd>
                  <a [routerLink]="['/jobs', origin]" class="akd-mono">{{ origin }}</a>
                </dd>
              }
              <dt>Created</dt>
              <dd class="akd-mono">{{ j.created_at | slice: 0 : 19 }}</dd>
              @if (j.finished_at; as finished) {
                <dt>Finished</dt>
                <dd class="akd-mono">{{ finished | slice: 0 : 19 }}</dd>
              }
              @if (j.dead_lettered_at; as dead) {
                <dt>Dead-lettered</dt>
                <dd class="akd-mono">{{ dead | slice: 0 : 19 }}</dd>
              }
            </dl>
          </akd-card>

          @if (remnants(); as items) {
            <akd-card title="Remnants on the server">
              <p class="remnants-intro akd-muted">
                This job left objects behind — forgetting it deletes nothing remotely. Clean them up
                by hand, or acknowledge them to close the job anyway (the acknowledgement is
                audited).
              </p>
              <ul class="remnants-list akd-mono">
                @for (item of items; track $index) {
                  <li>{{ item.field ? item.field + ': ' : '' }}{{ item.message }}</li>
                }
              </ul>
              <div>
                <button
                  class="akd-btn akd-btn--danger akd-btn--sm"
                  type="button"
                  [disabled]="busy()"
                  (click)="forgetAnyway(j)"
                >
                  Forget anyway (acknowledge remnants)
                </button>
              </div>
            </akd-card>
          }

          @if (j.steps?.length) {
            <akd-card title="Steps" [padded]="false">
              <table class="akd-table">
                <caption class="sr-only">
                  Steps of this job
                </caption>
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
            </akd-card>
          }

          @if (j.result) {
            <akd-card title="Result">
              <pre class="akd-mono block">{{ json(j.result) }}</pre>
            </akd-card>
          }

          @if (j.error; as jobError) {
            <akd-card title="Error">
              <p class="error-line">
                {{ jobError.message }} <span class="akd-muted">({{ jobError.code }})</span>
              </p>
              @if (jobError.details.length) {
                <pre class="akd-mono block">{{ json(jobError.details) }}</pre>
              }
            </akd-card>
          }
        </div>
      }
    </div>
  `,
  styles: [
    `
      .head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        min-width: 0;
      }
      .head h1 {
        font-family: var(--font-mono);
        overflow-wrap: anywhere;
      }
      .head-actions {
        display: flex;
        gap: var(--space-2);
      }
      .stack {
        display: grid;
        gap: var(--space-5);
        align-items: start;
      }
      .block {
        margin: 0;
        padding: var(--space-3);
        font-size: var(--text-sm);
        background: var(--bg-inset);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-2);
        overflow-x: auto;
      }
      .remnants-intro {
        margin: 0 0 var(--space-3);
      }
      .remnants-list {
        margin: 0 0 var(--space-3);
        padding-left: var(--space-5);
      }
      .error-line {
        margin: 0 0 var(--space-3);
      }
      .error-line:last-child {
        margin-bottom: 0;
      }
    `,
  ],
})
export class JobDetailComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

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
    if (
      !(await this.confirm.ask({
        title: 'Forget the job',
        message: 'Forget this job? It is closed as cancelled and leaves the dead-letter list.',
        confirmLabel: 'Forget',
      }))
    ) {
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
