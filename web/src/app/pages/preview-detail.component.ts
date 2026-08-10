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
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import {
  ActionsMenuComponent,
  type ActionItem,
} from '../../ui/actions-menu/actions-menu.component';
import {
  StackComponentsComponent,
  type StackComponentAction,
} from '../../ui/stack-components/stack-components.component';
import { ApplicationEnvsTabComponent } from './application/envs-tab.component';
import { TerminalComponent } from '../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../ui/terminal/protocol';
import { ApiService } from '../core/api.service';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { DEPLOYMENT_EVENTS, LiveEventsService, PREVIEW_EVENTS } from '../core/live-refresh';
import type { components } from '../../api/schema';

type Preview = components['schemas']['Preview'];
type LogLine = components['schemas']['LogLine'];
type ServiceComponent = components['schemas']['ServiceComponent'];
type ComponentMetric = components['schemas']['ComponentMetric'];
type Storage = components['schemas']['PersistentStorage'];

type TabId = 'overview' | 'logs' | 'terminal' | 'envs' | 'storages' | 'danger';

/**
 * Normalise a git remote (scp-like `git@host:owner/repo.git`, `ssh://…`, or an
 * http(s) URL) into the browsable https base — so a PR/commit path can be
 * appended. Empty stays empty. Mirror of the backend `browsableRepo` helper.
 */
function browsableRepo(raw: string | null | undefined): string {
  let s = (raw ?? '').trim();
  if (!s) return '';
  s = s.replace(/\.git$/, '');
  if (s.startsWith('git@')) {
    const rest = s.slice(4);
    const i = rest.indexOf(':');
    if (i >= 0) s = 'https://' + rest.slice(0, i) + '/' + rest.slice(i + 1).replace(/^\//, '');
  } else if (s.startsWith('ssh://')) {
    try {
      const u = new URL(s);
      s = 'https://' + u.host.replace(/^.*@/, '') + u.pathname;
    } catch {
      /* leave as-is */
    }
  } else if (s.startsWith('http://')) {
    s = 'https://' + s.slice('http://'.length);
  }
  return s.replace(/\/+$/, '');
}

/**
 * Everything of ONE PR instance, in the same tabbed layout as the
 * application page (§20.4): logs of its containers, its derived volumes,
 * the preview variable set, and its own danger zone — because debugging a
 * preview through production's pages meant debugging blind.
 */
@Component({
  selector: 'app-preview-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    CardComponent,
    IconComponent,
    StatusBadgeComponent,
    ActionsMenuComponent,
    StackComponentsComponent,
    ApplicationEnvsTabComponent,
    TerminalComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <header class="akd-bar head">
      <a
        [routerLink]="['/applications', uuid()]"
        class="akd-iconbtn akd-iconbtn--bordered"
        aria-label="Back to the application"
      >
        <akd-icon name="arrow-left" [size]="15" />
      </a>
      <h1 class="name">PR #{{ preview()?.pr_id ?? '…' }}</h1>
      @if (preview(); as p) {
        <akd-status-badge domain="preview" [state]="p.status" />
        @if (p.is_fork) {
          <span class="akd-badge akd-badge--accent">fork</span>
        }
        @if (p.fqdn) {
          <a class="akd-mono" [href]="'https://' + p.fqdn" target="_blank" rel="noopener">
            {{ p.fqdn }}
          </a>
        }
        <span class="spacer"></span>
        @if (p.source_branch) {
          @if (links()?.pr; as href) {
            <a
              class="akd-badge akd-badge--mono gitlink"
              [href]="href"
              target="_blank"
              rel="noopener"
              [title]="'Open PR #' + p.pr_id"
            >
              <akd-icon name="git-branch" [size]="13" />{{ p.source_branch }}
            </a>
          } @else {
            <span class="akd-badge akd-badge--mono">{{ p.source_branch }}</span>
          }
        }
        @if (p.head_sha; as sha) {
          @if (links()?.commit; as href) {
            <a
              class="akd-badge akd-badge--mono gitlink"
              [href]="href"
              target="_blank"
              rel="noopener"
            >
              {{ sha.slice(0, 8) }}
            </a>
          } @else {
            <span class="akd-badge akd-badge--mono">{{ sha.slice(0, 8) }}</span>
          }
        }
        @if (canManage() && p.status !== 'destroyed' && p.status !== 'destroying') {
          <button
            class="akd-btn akd-btn--secondary akd-btn--sm"
            type="button"
            [disabled]="busy()"
            title="Reset the inactivity TTL so this preview is not auto-destroyed"
            (click)="keepAlive()"
          >
            <akd-icon name="rotate-ccw" [size]="14" />
            Keep alive
          </button>
          <!-- Redeploying this instance never re-reads the pull request: the
               head SHA it runs is the one it was pinned to. -->
          <akd-actions-menu
            [items]="actions()"
            [disabled]="busy()"
            (selected)="run($any($event))"
          />
        }
      }
    </header>

    @if (notice(); as message) {
      <p class="akd-muted" role="status">{{ message }}</p>
    }

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <nav class="akd-tabs" role="tablist" aria-label="Preview sections">
      @for (t of tabs(); track t.id) {
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="tab() === t.id"
          [attr.aria-selected]="tab() === t.id"
          (click)="selectTab(t.id)"
        >
          {{ t.label }}
        </button>
      }
    </nav>

    @switch (tab()) {
      @case ('overview') {
        @if (preview(); as p) {
          <akd-card title="Instance">
            <dl class="facts">
              <div>
                <dt>Status</dt>
                <dd><akd-status-badge domain="preview" [state]="p.status" /></dd>
              </div>
              <div>
                <dt>Branch</dt>
                <dd class="akd-mono">
                  @if (p.source_branch; as b) {
                    @if (links()?.pr; as href) {
                      <a [href]="href" target="_blank" rel="noopener">{{ b }}</a>
                    } @else {
                      {{ b }}
                    }
                  } @else {
                    —
                  }
                </dd>
              </div>
              <div>
                <dt>Head</dt>
                <dd class="akd-mono">
                  @if (p.head_sha; as sha) {
                    @if (links()?.commit; as href) {
                      <a [href]="href" target="_blank" rel="noopener">{{ sha.slice(0, 8) }}</a>
                    } @else {
                      {{ sha.slice(0, 8) }}
                    }
                  } @else {
                    —
                  }
                </dd>
              </div>
              <div>
                <dt>URL</dt>
                <dd>
                  @if (p.fqdn) {
                    <a
                      class="akd-mono"
                      [href]="'https://' + p.fqdn"
                      target="_blank"
                      rel="noopener"
                      >{{ p.fqdn }}</a
                    >
                  } @else {
                    —
                  }
                </dd>
              </div>
              <div>
                <dt>Last deployed</dt>
                <dd>{{ p.last_deployed_at ?? '—' }}</dd>
              </div>
              <div>
                <dt>Fork</dt>
                <dd>
                  {{
                    p.is_fork
                      ? p.fork_approved
                        ? 'yes — approved'
                        : 'yes — pending approval'
                      : 'no'
                  }}
                </dd>
              </div>
            </dl>
          </akd-card>

          @if (single()) {
            <akd-stack-components
              class="stack"
              [single]="true"
              [components]="singleComponent()"
              [appName]="appName()"
              [pr]="p.pr_id"
              [metrics]="metrics()"
              (open)="onComponentAction($event)"
            />
          } @else if (components().length > 0) {
            <akd-stack-components
              class="stack"
              [components]="components()"
              [appName]="appName()"
              [pr]="p.pr_id"
              [metrics]="metrics()"
              (open)="onComponentAction($event)"
            />
          }
        }
      }
      @case ('logs') {
        <akd-card title="Container logs" [padded]="false">
          <div class="toolbar">
            @if (components().length > 0) {
              <div class="akd-select">
                <select
                  name="component"
                  class="akd-input"
                  [(ngModel)]="component"
                  (ngModelChange)="refreshLogs()"
                >
                  @for (c of components(); track c.name) {
                    <option [ngValue]="c.name">{{ c.name }}</option>
                  }
                </select>
              </div>
            }
            <div class="akd-select">
              <select
                name="lines"
                class="akd-input"
                [(ngModel)]="lines"
                (ngModelChange)="refreshLogs()"
              >
                <option [ngValue]="200">Last 200 lines</option>
                <option [ngValue]="500">Last 500 lines</option>
                <option [ngValue]="2000">Last 2000 lines</option>
              </select>
            </div>
            <label class="akd-check">
              <input
                type="checkbox"
                name="follow"
                [(ngModel)]="follow"
                (ngModelChange)="onFollow()"
              />
              Follow (refresh every 3 s)
            </label>
            <span class="spacer"></span>
            <button
              class="akd-btn akd-btn--secondary akd-btn--sm"
              type="button"
              [disabled]="busy()"
              (click)="refreshLogs()"
            >
              <akd-icon name="refresh-cw" [size]="13" />
              Refresh
            </button>
          </div>
          @if (logs(); as logLines) {
            @if (logLines.length === 0) {
              <p class="akd-muted pad">The container has not written anything yet.</p>
            } @else {
              <pre
                class="log"
              ><code>@for (line of logLines; track line.sequence) {{{ line.message }}
}</code></pre>
            }
          } @else if (busy()) {
            <p class="akd-muted pad">Loading…</p>
          }
        </akd-card>
      }
      @case ('terminal') {
        <section class="akd-card">
          <div class="akd-card__header">
            <h2 class="akd-card__title">Terminal</h2>
            @if (components().length > 0) {
              <div class="akd-select">
                <select name="terminalComponent" class="akd-input" [(ngModel)]="terminalComponent">
                  @for (c of components(); track c.name) {
                    <option [ngValue]="c.name">{{ c.name }}</option>
                  }
                </select>
              </div>
            }
            <span class="spacer"></span>
            <span class="akd-muted note-inline"
              >opening and closing are audited · keystrokes are never logged</span
            >
          </div>
          <div class="akd-card__body">
            <akd-terminal
              title="Preview shell"
              hint="Opens a shell in the preview's container — an ephemeral instance, destroyed with the PR."
              [open]="openTerminalSession"
            />
          </div>
        </section>
      }
      @case ('envs') {
        <p class="akd-muted note">
          The EFFECTIVE variables of this PR: the shared preview set plus this preview's own
          overrides (INV-010: production values are never inherited). Adding or editing here creates
          an override for THIS PR only; changes apply on its next deployment.
        </p>
        <app-application-envs-tab [uuid]="uuid()" [previewUuid]="previewUuid()" />
      }
      @case ('storages') {
        <akd-card title="Preview storages" [padded]="false">
          <p class="akd-muted pad">
            Derived from the application's storages — created empty (or cloned when the volume
            declares preview_seed), destroyed with the preview.
          </p>
          @if (storages().length === 0) {
            <p class="akd-muted pad">No persistent storage declared.</p>
          } @else {
            <table class="akd-table">
              <thead>
                <tr>
                  <th scope="col">Volume</th>
                  <th scope="col">Mount path</th>
                </tr>
              </thead>
              <tbody>
                @for (s of storages(); track s.uuid) {
                  <tr>
                    <td class="akd-mono">{{ previewVolumeName(s) }}</td>
                    <td class="akd-mono">{{ s.mount_path }}</td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      }
      @case ('danger') {
        <akd-card title="Danger zone">
          <div class="danger">
            <div>
              <strong>Destroy this preview</strong>
              <p class="akd-muted">
                Removes its containers, volumes, networks and routing. Production is never touched.
                The PR stays open — a /deploy comment or a push recreates a fresh instance.
              </p>
            </div>
            <button
              class="akd-btn akd-btn--danger"
              type="button"
              [disabled]="busy() || preview()?.status === 'destroyed'"
              (click)="destroy()"
            >
              Destroy preview
            </button>
          </div>
        </akd-card>
      }
    }
  `,
  styles: [
    `
      .head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        margin-bottom: var(--space-4);
      }
      .name {
        margin: 0;
        font-size: var(--text-lg);
      }
      .spacer {
        flex: 1;
      }
      a.gitlink {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
        text-decoration: none;
      }
      a.gitlink:hover {
        text-decoration: underline;
      }
      .akd-tabs {
        margin-bottom: var(--space-4);
      }
      .stack {
        display: block;
        margin-top: var(--space-5);
      }
      .facts {
        margin: 0;
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
        gap: var(--space-4);
      }
      .facts dt {
        font-size: var(--text-xs);
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--text-3);
        margin-bottom: var(--space-1);
      }
      .facts dd {
        margin: 0;
        overflow-wrap: anywhere;
      }
      .toolbar {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-3);
        border-bottom: 1px solid var(--border);
      }
      .pad {
        padding: var(--space-3);
      }
      .note {
        margin: 0 0 var(--space-3);
      }
      .note-inline {
        font-size: var(--text-xs);
      }
      .log {
        margin: 0;
        padding: var(--space-3);
        max-height: 60vh;
        overflow: auto;
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        line-height: 1.5;
        white-space: pre-wrap;
        word-break: break-all;
        background: var(--log-bg, var(--surface-2));
      }
      .danger {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-4);
      }
    `,
  ],
})
export class PreviewDetailComponent {
  readonly uuid = input.required<string>();
  readonly previewUuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);
  private readonly router = inject(Router);

  /**
   * Each tab names the permission it is useless without. Logs and storages ride
   * on the permissions that already open this page (previews:read covers the
   * preview's logs by contract); a reviewer (ADR-059) loses the shell, the
   * variables and the danger zone.
   */
  private readonly allTabs: readonly { id: TabId; label: string; permission?: string }[] = [
    { id: 'overview', label: 'Overview' },
    { id: 'logs', label: 'Logs' },
    { id: 'terminal', label: 'Terminal', permission: 'terminal:open' },
    { id: 'envs', label: 'Environment variables', permission: 'secrets:read' },
    { id: 'storages', label: 'Storages' },
    { id: 'danger', label: 'Danger', permission: 'previews:manage' },
  ];
  protected readonly tabs = computed(() =>
    this.allTabs.filter((t) => !t.permission || this.api.can(t.permission)),
  );
  /** Keep-alive, redeploy and destroy stay with previews:manage. */
  protected canManage(): boolean {
    return this.api.can('previews:manage');
  }
  protected readonly tab = signal<TabId>('overview');
  /** The active tab lives in the URL (?tab=…): a refresh keeps it, and
   * back/forward walk the tabs — withComponentInputBinding feeds this input
   * from the query parameter on every navigation. */
  readonly tabParam = input<string | undefined>(undefined, { alias: 'tab' });

  private readonly activatedRoute = inject(ActivatedRoute);

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      queryParams: { tab: id === 'overview' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  /** A stack-components action: open that service's logs/shell/volumes here. */
  protected onComponentAction(action: StackComponentAction): void {
    switch (action.target) {
      case 'logs':
        this.component = action.component;
        this.selectTab('logs');
        this.refreshLogs();
        break;
      case 'terminal':
        this.terminalComponent = action.component;
        this.selectTab('terminal');
        break;
      case 'storages':
        this.selectTab('storages');
        break;
    }
  }

  protected readonly preview = signal<Preview | null>(null);
  /** Name of the parent application — what the CLI's `app` verbs take as their
   *  trailing argument (ADR-070 §1). */
  protected readonly appName = signal<string>('');
  /** Build pack of the parent app — single-container unless 'compose'. */
  protected readonly appBuildPack = signal<string | null>(null);
  /** Raw git remote of the parent app (git source) — used to build forge links. */
  private readonly appRepo = signal<string | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);

  /**
   * Browsable forge links for this PR: the pull request itself and the head
   * commit. Built from the app's git remote (normalised to https) + the
   * preview's provider/pr_id/head_sha. Null when the data is missing or the
   * forge is unknown.
   */
  protected readonly links = computed(() => {
    const p = this.preview();
    const base = browsableRepo(this.appRepo());
    if (!p || !base) return null;
    const paths: Record<string, { commit: string; pr: string }> = {
      github: { commit: '/commit/', pr: '/pull/' },
      gitlab: { commit: '/-/commit/', pr: '/-/merge_requests/' },
      gitea: { commit: '/commit/', pr: '/pulls/' },
      bitbucket: { commit: '/commits/', pr: '/pull-requests/' },
    };
    const forge = p.provider ? paths[p.provider] : undefined;
    return {
      repo: base,
      pr: forge && p.pr_id ? base + forge.pr + p.pr_id : null,
      commit: forge && p.head_sha ? base + forge.commit + p.head_sha : null,
    };
  });

  /** A single-container preview (no compose services): show one instance panel. */
  protected readonly single = computed(() => !!this.preview() && this.appBuildPack() !== 'compose');
  protected readonly singleComponent = computed<ServiceComponent[]>(() => {
    const p = this.preview();
    if (!p || !this.single()) return [];
    return [
      {
        uuid: p.uuid,
        name: this.appName() || 'app',
        observed_status: p.status === 'active' ? 'running' : 'stopped',
        is_database: false,
        exclude_from_hc: false,
        created_at: p.created_at ?? '',
      } as unknown as ServiceComponent,
    ];
  });
  /** Live per-service stats (ADR-034), polled while the overview is open. */
  protected readonly metrics = signal<Record<string, ComponentMetric>>({});
  protected readonly storages = signal<Storage[]>([]);
  protected readonly logs = signal<LogLine[] | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected lines = 200;
  protected component = '';
  protected terminalComponent = '';
  protected follow = false;
  private timer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    // URL → state: seeds the tab on load and follows back/forward — the
    // navigation history is the source of truth for which tab is open.
    effect(() => {
      const wanted = this.tabParam();
      const valid = this.tabs().find((t) => t.id === wanted)?.id;
      this.tab.set(valid ?? 'overview');
    });
    effect(() => {
      const app = this.uuid();
      const preview = this.previewUuid();
      untracked(() => void this.init(app, preview));
    });
    // Live metrics: poll only while the overview shows an instance panel.
    effect((onCleanup) => {
      const app = this.uuid();
      const preview = this.previewUuid();
      const hasPanel = this.single() ? !!this.preview() : this.components().length > 0;
      if (this.tab() !== 'overview' || !hasPanel) {
        this.metrics.set({});
        return;
      }
      const fallback = this.appName() || 'app';
      let stopped = false;
      const poll = async () => {
        try {
          const page = await this.api.client().getPreviewMetrics(app, preview);
          if (!stopped) {
            this.metrics.set(
              Object.fromEntries(page.data.map((m) => [m.component || fallback, m])),
            );
          }
        } catch {
          // Transient — keep the last snapshot on screen.
        }
      };
      void poll();
      const timer = setInterval(poll, 4000);
      onCleanup(() => {
        stopped = true;
        clearInterval(timer);
      });
    });
    // Live refresh (ADR-024/040): preview status and component states move on
    // their own — deploys, sleep/wake by the scheduler or the agent. One
    // shared stream for the whole app (LiveEventsService).
    const live = inject(LiveEventsService);
    effect((onCleanup) => {
      const app = this.uuid();
      const preview = this.previewUuid();
      onCleanup(
        live.refresh(
          [...PREVIEW_EVENTS, ...DEPLOYMENT_EVENTS],
          (ev) => ev.resource_uuid === app,
          () => untracked(() => void this.refreshState(app, preview)),
        ),
      );
    });
    inject(DestroyRef).onDestroy(() => this.stopFollow());
  }

  /**
   * Light event-driven refresh: preview + component state only — the logs,
   * the active tab and the user's component selections stay untouched.
   */
  private async refreshState(app: string, previewUuid: string): Promise<void> {
    try {
      const [previews, comps] = await Promise.all([
        this.api.client().listApplicationPreviews(app),
        this.api.client().listApplicationComponents(app),
      ]);
      const preview = previews.data.find((p) => p.uuid === previewUuid);
      if (preview) this.preview.set(preview);
      this.components.set(comps.data);
    } catch {
      // Transient — keep the last state on screen.
    }
  }

  protected previewVolumeName(s: Storage): string {
    return s.kind === 'volume' ? `${this.previewUuid()}_${s.name ?? ''}` : (s.host_path ?? '');
  }

  private async init(app: string, previewUuid: string): Promise<void> {
    try {
      const [application, previews, comps, storages] = await Promise.all([
        this.api.client().getApplication(app),
        this.api.client().listApplicationPreviews(app),
        this.api.client().listApplicationComponents(app),
        this.api.client().listApplicationStorages(app),
      ]);
      const preview = previews.data.find((p) => p.uuid === previewUuid) ?? null;
      if (!preview) {
        this.error.set('Preview not found — it may have been removed.');
        return;
      }
      this.appName.set(application.name);
      this.appBuildPack.set(application.build_pack ?? null);
      this.appRepo.set(application.git_repository ?? null);
      this.preview.set(preview);
      this.components.set(comps.data);
      this.storages.set(storages.data);
      if (comps.data.length > 0) {
        this.component = comps.data[0].name;
        this.terminalComponent = comps.data[0].name;
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
      return;
    }
    await this.loadLogs();
  }

  protected readonly openTerminalSession = async (): Promise<TerminalSessionInfo> =>
    (await this.api
      .client()
      .createPreviewTerminalSession(
        this.uuid(),
        this.previewUuid(),
        this.terminalComponent ? { component: this.terminalComponent } : undefined,
      )) as unknown as TerminalSessionInfo;

  protected refreshLogs(): void {
    void this.loadLogs();
  }

  protected onFollow(): void {
    this.stopFollow();
    if (this.follow) {
      this.timer = setInterval(() => void this.loadLogs(), 3000);
    }
  }

  private stopFollow(): void {
    if (this.timer !== null) {
      clearInterval(this.timer);
      this.timer = null;
    }
  }

  private async loadLogs(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      const page = await this.api.client().getPreviewLogs(this.uuid(), this.previewUuid(), {
        lines: this.lines,
        ...(this.component ? { component: this.component } : {}),
      });
      this.logs.set(page.data);
      this.error.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.follow = false;
      this.stopFollow();
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * What can be done to this instance. A PR preview has its own variable set
   * (INV-010), and editing one only reaches the container when the container
   * is created again — which is what "Recreate" does, without rebuilding
   * anything (ADR-048).
   */
  protected readonly actions = computed<ActionItem[]>(() => {
    // A fork's code runs only after a maintainer's explicit yes (§20.4.8):
    // until then there is nothing to redeploy.
    const forkPending = this.preview()?.is_fork && !this.preview()?.fork_approved;
    return [
      {
        id: 'redeploy',
        label: 'Redeploy',
        icon: 'rocket',
        disabled: !!forkPending,
        hint: 'Build and deploy this PR again at the commit it already runs',
      },
      {
        id: 'rebuild',
        label: 'Rebuild (no cache)',
        icon: 'hammer',
        disabled: !!forkPending,
        hint: 'Same, ignoring every cached layer',
      },
      {
        id: 'recreate',
        label: 'Recreate (apply config)',
        icon: 'settings-2',
        disabled: !!forkPending,
        hint: "Redeploy the running image with this preview's current variables — no build",
      },
    ];
  });

  /** Queues one of the menu's deployments and follows it — the log streams
   *  on the deployment page, which is what one asked for it to watch. */
  protected async run(action: 'redeploy' | 'rebuild' | 'recreate'): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const { deployment_uuid } = await this.api
        .client()
        .redeployPreview(this.uuid(), this.previewUuid(), {
          forceRebuild: action === 'rebuild',
          skipBuild: action === 'recreate',
        });
      if (deployment_uuid) {
        void this.router.navigate(['/applications', this.uuid(), 'deployments', deployment_uuid]);
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /** Reset the inactivity TTL so the reaper leaves this preview alone. */
  protected async keepAlive(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().keepPreview(this.uuid(), this.previewUuid());
      this.notice.set('Inactivity TTL reset — this preview will not be auto-destroyed for now.');
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async destroy(): Promise<void> {
    const p = this.preview();
    if (!p) return;
    if (
      !(await this.confirm.ask({
        title: 'Destroy the preview',
        message: `Destroy the preview of PR #${p.pr_id}? Its containers and volumes will be removed.`,
        confirmLabel: 'Destroy',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    try {
      await this.api.client().destroyPreview(this.uuid(), this.previewUuid());
      await this.router.navigate(['/applications', this.uuid()]);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
