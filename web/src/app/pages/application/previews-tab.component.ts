import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  type OnDestroy,
  signal,
  untracked,
} from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { ModalComponent } from '../../../ui/modal/modal.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../../ui/status-badge/status-badge.component';
import { ApiService } from '../../core/api.service';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import { DEPLOYMENT_EVENTS, LiveEventsService, PREVIEW_EVENTS } from '../../core/live-refresh';
import type { components } from '../../../api/schema';

type Preview = components['schemas']['Preview'];
type PullRequestInfo = components['schemas'] extends never
  ? never
  : {
      number: number;
      title: string;
      branch: string;
      head_sha: string;
      is_fork: boolean;
      draft?: boolean;
    };

/**
 * PR previews (§20.4): one ephemeral instance per pull request, protected by
 * default. A fork PR deploys NOTHING until a maintainer approves it here
 * (INV-010) — that button is the approval.
 */
@Component({
  selector: 'app-application-previews-tab',
  standalone: true,
  imports: [
    RouterLink,
    FormsModule,
    CardComponent,
    EmptyStateComponent,
    ModalComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (canManage()) {
      <div class="actions-bar">
        <!-- The platform-side /deploy (§20.4.7): pick any open PR of the
             repository and give it an instance, without leaving the page. -->
        <button
          class="akd-btn akd-btn--primary akd-btn--sm"
          type="button"
          [disabled]="busy()"
          (click)="openDeployModal()"
        >
          <akd-icon name="rocket" [size]="14" />
          Deploy a PR
        </button>
      </div>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (previews().length === 0) {
      <akd-empty-state
        icon="git-branch"
        title="No preview yet"
        message="Enable previews in Settings, use a GitHub App source, and every pull request gets its own protected instance — destroyed on merge, close or TTL."
      />
    } @else {
      <akd-card title="Pull request previews" [padded]="false">
        <table class="akd-table akd-table--clickable">
          <caption class="sr-only">
            PR previews of this application
          </caption>
          <thead>
            <tr>
              <th scope="col">PR</th>
              <th scope="col">Branch</th>
              <th scope="col">Status</th>
              <th scope="col">URL</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (p of previews(); track p.uuid) {
              <tr
                tabindex="0"
                role="link"
                [attr.aria-label]="'Open preview for pull request ' + p.pr_id"
                (click)="open(p)"
                (keydown.enter)="openFromKeyboard($event, p)"
                (keydown.space)="openFromKeyboard($event, p)"
              >
                <td>
                  <!-- The PR badge opens the preview's own page: logs,
                       storages, preview variables and its danger zone. -->
                  <a
                    class="akd-badge akd-badge--mono"
                    [routerLink]="['/applications', uuid(), 'previews', p.uuid]"
                    (click)="$event.stopPropagation()"
                    >#{{ p.pr_id }}</a
                  >
                </td>
                <td class="akd-mono">
                  {{ p.source_branch ?? '—' }}
                  @if (p.is_fork) {
                    <span class="akd-muted">(fork)</span>
                  }
                </td>
                <td>
                  @if (awaitingDeploy(p)) {
                    <!-- Manual-first policy: the row is a RESERVATION — the
                         internal status stays 'queued', but nothing will
                         deploy it until a human says so (§20.4). -->
                    <span class="akd-badge akd-badge--mono">awaiting deploy</span>
                  } @else {
                    <akd-status-badge domain="preview" [state]="p.status" />
                  }
                </td>
                <td>
                  @if (p.fqdn) {
                    <a
                      class="akd-mono"
                      [href]="'https://' + p.fqdn"
                      target="_blank"
                      rel="noopener"
                      (click)="$event.stopPropagation()"
                    >
                      {{ p.fqdn }}
                    </a>
                  } @else {
                    <span class="akd-muted">—</span>
                  }
                </td>
                <td class="right" (click)="$event.stopPropagation()">
                  @if (canManage() && awaitingDeploy(p) && !p.is_fork && p.pr_id) {
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="deployPr(p.pr_id)"
                    >
                      Deploy
                    </button>
                  }
                  <a
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    [routerLink]="['/applications', uuid(), 'previews', p.uuid]"
                  >
                    Details
                  </a>
                  @if (canManage() && p.is_fork && !p.fork_approved && p.status !== 'destroyed') {
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="approve(p)"
                    >
                      Approve fork
                    </button>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }

    <akd-modal
      [open]="deployOpen()"
      title="Deploy a pull request preview"
      (closed)="deployOpen.set(false)"
    >
      <div class="modal-body">
        <input
          class="akd-input"
          name="prSearch"
          placeholder="Search by number, title or branch…"
          [(ngModel)]="prSearch"
        />
        @if (prsLoading()) {
          <p class="akd-muted">Loading open pull requests…</p>
        } @else if (prsError(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        } @else if (filteredPrs().length === 0) {
          <p class="akd-muted">No open pull request matches.</p>
        } @else {
          <ul class="pr-list">
            @for (pr of filteredPrs(); track pr.number) {
              <li>
                <span class="akd-badge akd-badge--mono">#{{ pr.number }}</span>
                <span class="pr-title">{{ pr.title }}</span>
                <span class="akd-mono akd-muted">{{ pr.branch }}</span>
                @if (pr.is_fork) {
                  <span class="akd-badge akd-badge--accent">fork</span>
                }
                @if (pr.draft) {
                  <span class="akd-badge">draft</span>
                }
                <span class="spacer"></span>
                <button
                  class="akd-btn akd-btn--primary akd-btn--sm"
                  type="button"
                  [disabled]="busy()"
                  (click)="deployPr(pr.number)"
                >
                  Deploy
                </button>
              </li>
            }
          </ul>
        }
      </div>
    </akd-modal>
  `,
  styles: [
    `
      .actions-bar {
        display: flex;
        justify-content: flex-end;
        margin-bottom: var(--space-3);
      }
      .modal-body {
        display: grid;
        gap: var(--space-3);
      }
      .pr-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: grid;
        gap: var(--space-2);
        max-height: 50vh;
        overflow: auto;
      }
      .pr-list li {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        padding: var(--space-2);
        border: 1px solid var(--border);
        border-radius: var(--radius-md);
      }
      .pr-title {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 18rem;
      }
      .spacer {
        flex: 1;
      }
    `,
  ],
})
export class ApplicationPreviewsTabComponent implements OnDestroy {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);
  private readonly router = inject(Router);

  /** The application's preview_deploy_on_open — fed by the parent page so a
   * manual-first reservation can be labeled for what it is. */
  readonly deployOnOpen = input<boolean | undefined>(undefined);

  /**
   * Manual-first reservation (§20.4): the internal status is 'queued', but
   * with auto-deploy off, no deployment ever ran and no human ordered one,
   * nothing will pick it up — the honest label is "awaiting deploy".
   */
  protected awaitingDeploy(p: Preview): boolean {
    return (
      this.deployOnOpen() === false &&
      p.status === 'queued' &&
      !p.deploy_requested_at &&
      !p.last_deployed_at
    );
  }

  /** A reviewer (ADR-059) reads previews and follows their links; deploying a
   * PR and approving a fork stay with previews:manage. */
  protected canManage(): boolean {
    return this.api.can('previews:manage');
  }

  protected readonly previews = signal<Preview[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly deployOpen = signal(false);
  protected readonly prs = signal<PullRequestInfo[]>([]);
  protected readonly prsLoading = signal(false);
  protected readonly prsError = signal<string | null>(null);
  protected prSearch = '';

  private readonly live = inject(LiveEventsService);
  private readonly stopLive: () => void;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
    // Live refresh (ADR-024): the list reloads on any event touching THIS
    // application, so preview states (deploying, active, sleeping, waking…)
    // move without a manual page refresh — on the app's ONE shared stream.
    this.stopLive = this.live.refresh(
      [...PREVIEW_EVENTS, ...DEPLOYMENT_EVENTS],
      (ev) => ev.resource_uuid === this.uuid(),
      () => untracked(() => void this.load(this.uuid())),
    );
  }

  ngOnDestroy(): void {
    this.stopLive();
  }

  protected open(preview: Preview): void {
    void this.router.navigate(['/applications', this.uuid(), 'previews', preview.uuid]);
  }

  protected openFromKeyboard(event: Event, preview: Preview): void {
    if (event.target !== event.currentTarget) return;
    event.preventDefault();
    this.open(preview);
  }

  private async load(uuid: string): Promise<void> {
    try {
      const page = await this.api.client().listApplicationPreviews(uuid);
      this.previews.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected filteredPrs(): PullRequestInfo[] {
    const q = this.prSearch.trim().toLowerCase();
    if (!q) return this.prs();
    return this.prs().filter(
      (pr) =>
        String(pr.number).includes(q) ||
        pr.title.toLowerCase().includes(q) ||
        pr.branch.toLowerCase().includes(q),
    );
  }

  protected openDeployModal(): void {
    this.deployOpen.set(true);
    this.prSearch = '';
    this.prsError.set(null);
    this.prsLoading.set(true);
    this.api
      .client()
      .listApplicationPullRequests(this.uuid())
      .then((page) => this.prs.set(page.data))
      .catch((err) => this.prsError.set(ApiService.describe(err)))
      .finally(() => this.prsLoading.set(false));
  }

  protected async deployPr(prNumber: number): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      await this.api.client().deployPreviewForPr(this.uuid(), prNumber);
      this.deployOpen.set(false);
      await this.load(this.uuid());
    } catch (err) {
      this.prsError.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async approve(preview: Preview): Promise<void> {
    if (!preview.uuid || this.busy()) return;
    if (
      !(await this.confirm.ask({
        title: 'Approve the fork preview',
        message: `Approve the preview of fork PR #${preview.pr_id}? Its code will be built on this server — no secret is ever injected.`,
        confirmLabel: 'Approve',
        danger: false,
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().approvePreviewFork(this.uuid(), preview.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
