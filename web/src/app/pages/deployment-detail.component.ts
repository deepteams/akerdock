import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { RouterLink } from '@angular/router';
import { SlicePipe } from '@angular/common';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { CardComponent } from '../../ui/card/card.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Deployment = components['schemas']['Deployment'];
type LogLine = components['schemas']['LogLine'];

/** A gap is rendered like a line, because the operator must SEE it (§22.2). */
interface GapMarker {
  sequence: number;
  gap: true;
}
type Row = LogLine | GapMarker;

const isGap = (row: Row): row is GapMarker => 'gap' in row;

const TERMINAL: Deployment['status'][] = ['succeeded', 'failed', 'cancelled', 'superseded'];

/**
 * One deployment's build log, on its own page (§22): the logs of a build are a
 * stream to read, not a widget squeezed under the overview. Opened from a row in
 * the Deployments tab; the stream resumes on reconnect via Last-Event-ID.
 */
@Component({
  selector: 'app-deployment-detail',
  standalone: true,
  imports: [RouterLink, SlicePipe, StatusBadgeComponent, IconComponent, CardComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <header class="akd-bar head">
      <a
        [routerLink]="['/applications', uuid()]"
        [queryParams]="{ tab: 'deployments' }"
        class="akd-iconbtn akd-iconbtn--bordered"
        aria-label="Back to deployments"
      >
        <akd-icon name="arrow-left" [size]="15" />
      </a>
      <h1 class="name">Deployment</h1>
      @if (deployment(); as d) {
        <akd-status-badge domain="deployment" [state]="d.status" />
        @if (d.commit_sha) {
          @if (links()?.commit; as href) {
            <a class="akd-badge akd-badge--mono gitlink" [href]="href" target="_blank" rel="noopener">
              {{ d.commit_sha | slice: 0 : 8 }}
            </a>
          } @else {
            <span class="akd-badge akd-badge--mono">{{ d.commit_sha | slice: 0 : 8 }}</span>
          }
        }
        @if (d.branch) {
          @if (links()?.branch; as href) {
            <a class="akd-muted gitlink" [href]="href" target="_blank" rel="noopener">
              <akd-icon name="git-branch" [size]="13" />{{ d.branch }}
            </a>
          } @else {
            <span class="akd-muted"><akd-icon name="git-branch" [size]="13" />{{ d.branch }}</span>
          }
        }
        @if (links()?.repo; as href) {
          <a
            class="akd-iconbtn akd-iconbtn--bordered akd-iconbtn--sm"
            [href]="href"
            target="_blank"
            rel="noopener"
            aria-label="Open repository"
          >
            <akd-icon name="external-link" [size]="13" />
          </a>
        }
        <span class="akd-muted">
          {{ d.trigger }}@if (d.pr_id) {
            @if (links()?.pr; as href) {
              <a class="gitlink" [href]="href" target="_blank" rel="noopener">&nbsp;#{{ d.pr_id }}</a>
            } @else {
              <span>&nbsp;#{{ d.pr_id }}</span>
            }
          }{{ d.is_rollback ? ' · rollback' : '' }}
        </span>
        <span class="spacer"></span>
        <span class="akd-muted when">{{ duration(d) }}</span>
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
      }
    </header>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (deployment(); as d) {
      @if (d.commit_message || d.commit_author) {
        <p class="akd-muted commit">
          @if (d.commit_message) {
            <span>{{ d.commit_message }}</span>
          }
          @if (d.commit_author) {
            <span class="author"
              ><akd-icon name="users" [size]="13" />{{ d.commit_author }}</span
            >
          }
        </p>
      }
    }

    <akd-card title="Build logs" [padded]="false">
      <span card-actions>
        @if (streaming()) {
          <akd-status-badge domain="job" state="running" label="live · SSE" />
        }
      </span>
      @if (rows().length === 0) {
        <p class="akd-muted pad">
          {{
            streaming() ? 'Waiting for the first log line…' : 'This deployment produced no logs.'
          }}
        </p>
      } @else {
        <div class="akd-log logpane" tabindex="0" aria-label="Deployment build logs">
          @for (row of rows(); track row.sequence) {
            @if (isGap(row)) {
              <div class="akd-log__line akd-log__line--warn">
                <span class="akd-log__msg">{{ render(row) }}</span>
              </div>
            } @else {
              <div
                class="akd-log__line"
                [class.akd-log__line--error]="row.channel === 'stderr'"
                [class.akd-log__line--cmd]="row.channel === 'system'"
              >
                <span class="akd-log__ts">{{ clock(row.timestamp) }}</span>
                <span class="akd-log__msg">{{ render(row) }}</span>
              </div>
            }
          }
        </div>
      }
    </akd-card>
  `,
  styles: [
    `
      .head {
        justify-content: flex-start;
        row-gap: var(--space-2);
      }
      .spacer {
        flex: 1;
      }
      .when {
        font-size: var(--text-sm);
        font-family: var(--font-mono);
      }
      .commit {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--space-2) var(--space-3);
        margin: 0 0 var(--space-4);
      }
      .commit .author {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
        font-size: var(--text-sm);
      }
      .gitlink {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
        text-decoration: none;
        color: inherit;
      }
      a.gitlink:hover {
        text-decoration: underline;
        color: var(--text-1);
      }
      .akd-iconbtn--sm {
        width: 26px;
        height: 26px;
      }
      .pad {
        margin: 0;
        padding: var(--space-4) var(--space-5);
      }
      .logpane {
        border: none;
        border-radius: 0 0 var(--radius-3) var(--radius-3);
        max-height: 72vh;
        padding: var(--space-2) 0;
      }
      .logpane:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
    `,
  ],
})
export class DeploymentDetailComponent {
  /** Both bound from the route by withComponentInputBinding. */
  readonly uuid = input.required<string>();
  readonly deploymentUuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly deployment = signal<Deployment | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly streaming = signal(false);

  /**
   * The lines, keyed by sequence. The sequence IS the identity of a line
   * (§27.24), so a reconnect that replays what we already have overwrites it
   * instead of duplicating it — what makes Last-Event-ID resumption safe.
   */
  private readonly lines = signal<Map<number, Row>>(new Map());
  protected readonly rows = computed(() =>
    [...this.lines().values()].sort((a, b) => a.sequence - b.sequence),
  );

  private source: EventSource | null = null;

  protected isGap = isGap;

  constructor() {
    inject(DestroyRef).onDestroy(() => this.closeStream());
    // The route inputs are not readable in the constructor: wait for the router
    // to bind them, then load the metadata and open the stream.
    effect(() => {
      const deploymentUuid = this.deploymentUuid();
      untracked(() => {
        void this.loadDeployment(deploymentUuid);
        this.openStream(deploymentUuid);
      });
    });
  }

  protected cancellable(d: Deployment): boolean {
    return !TERMINAL.includes(d.status);
  }

  /**
   * Browsable forge links derived from repository_url + provider. The path
   * conventions differ per forge (github `/tree`, gitlab `/-/tree`, …); an
   * unknown forge still gets the plain repository link, just no deep links.
   */
  protected readonly links = computed(() => {
    const d = this.deployment();
    const base = d?.repository_url;
    if (!base) return null;
    const paths: Record<string, { branch: string; commit: string; pr: string }> = {
      github: { branch: '/tree/', commit: '/commit/', pr: '/pull/' },
      gitlab: { branch: '/-/tree/', commit: '/-/commit/', pr: '/-/merge_requests/' },
      gitea: { branch: '/src/branch/', commit: '/commit/', pr: '/pulls/' },
      bitbucket: { branch: '/src/', commit: '/commits/', pr: '/pull-requests/' },
    };
    const p = d.provider ? paths[d.provider] : undefined;
    const enc = (s: string) => s.split('/').map(encodeURIComponent).join('/');
    const pr = p && d.pr_id ? base + p.pr + d.pr_id : null;
    return {
      repo: base,
      // A preview deployment: send the branch straight to its PR — that is what
      // is actually useful. A plain branch deploy links to the branch tree.
      branch: pr ?? (p && d.branch ? base + p.branch + enc(d.branch) : null),
      commit: p && d.commit_sha ? base + p.commit + d.commit_sha : null,
      pr,
    };
  });

  /** Derived from started_at/finished_at — a deployment still running has none. */
  protected duration(d: Deployment): string {
    if (!d.started_at || !d.finished_at) return d.started_at ? 'running…' : '—';
    const ms = Date.parse(d.finished_at) - Date.parse(d.started_at);
    if (!Number.isFinite(ms) || ms < 0) return '—';
    const seconds = Math.round(ms / 1000);
    return seconds >= 60 ? `${Math.floor(seconds / 60)}m ${seconds % 60}s` : `${seconds}s`;
  }

  protected render(row: Row): string {
    if (isGap(row))
      return '⚠ lines dropped by the server (backpressure) — the log is incomplete here';
    const prefix = row.channel === 'stderr' ? '! ' : row.channel === 'system' ? '· ' : '  ';
    return prefix + row.message;
  }

  /** HH:MM:SS from an RFC 3339 timestamp. */
  protected clock(timestamp: string): string {
    return timestamp.slice(11, 19);
  }

  private async loadDeployment(deploymentUuid: string): Promise<void> {
    try {
      this.deployment.set(await this.api.client().getDeployment(deploymentUuid));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  private openStream(deploymentUuid: string): void {
    this.lines.set(new Map());
    this.closeStream();

    const source = this.api.client().deploymentLogs(deploymentUuid);
    this.source = source;
    this.streaming.set(true);

    source.addEventListener('log', (event) => {
      const line = JSON.parse((event as MessageEvent<string>).data) as LogLine;
      this.lines.update((map) => new Map(map).set(line.sequence, line));
    });
    // A gap is not an error to swallow: the server is telling us it dropped
    // lines. Hiding that would leave the operator reading a log that looks
    // complete and is not.
    source.addEventListener('gap', (event) => {
      const id = Number((event as MessageEvent).lastEventId) || this.rows().length;
      this.lines.update((map) => new Map(map).set(id + 0.5, { sequence: id + 0.5, gap: true }));
    });
    // `end` means the deployment reached a terminal status: the stream is over
    // on purpose, so close it and refresh the final status/duration.
    source.addEventListener('end', () => {
      this.closeStream();
      void this.loadDeployment(deploymentUuid);
    });
  }

  private closeStream(): void {
    this.source?.close();
    this.source = null;
    this.streaming.set(false);
  }

  protected async cancel(d: Deployment): Promise<void> {
    if (!confirm('Cancel this deployment? The currently routed container stays untouched.')) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().cancelDeployment(d.uuid);
      await this.loadDeployment(d.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
