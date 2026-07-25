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
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { CardComponent } from '../../ui/card/card.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';
import { ApplicationSettingsTabComponent } from './application/settings-tab.component';
import { ApplicationEnvsTabComponent } from './application/envs-tab.component';
import { ApplicationStoragesTabComponent } from './application/storages-tab.component';
import { ApplicationTasksTabComponent } from './application/tasks-tab.component';
import { ApplicationDeploymentsTabComponent } from './application/deployments-tab.component';
import { ApplicationWebhookTabComponent } from './application/webhook-tab.component';
import { ApplicationDangerTabComponent } from './application/danger-tab.component';
import { ApplicationPreviewsTabComponent } from './application/previews-tab.component';
import { ApplicationTerminalTabComponent } from './application/terminal-tab.component';
import { ApplicationLogsTabComponent } from './application/logs-tab.component';

type Application = components['schemas']['Application'];
type Deployment = components['schemas']['Deployment'];
type ServiceComponent = components['schemas']['ServiceComponent'];
type LogLine = components['schemas']['LogLine'];

type TabId =
  | 'overview'
  | 'settings'
  | 'envs'
  | 'storages'
  | 'tasks'
  | 'deployments'
  | 'logs'
  | 'previews'
  | 'terminal'
  | 'webhook'
  | 'danger';

/** A gap is rendered like a line, because the operator must SEE it (§22.2). */
interface GapMarker {
  sequence: number;
  gap: true;
}
type Row = LogLine | GapMarker;

const isGap = (row: Row): row is GapMarker => 'gap' in row;

/** Default listen port of a database engine, for a ready-to-run port-forward. */
function enginePort(engine: string | null | undefined): number | null {
  switch (engine) {
    case 'postgresql':
      return 5432;
    case 'mysql':
    case 'mariadb':
      return 3306;
    case 'mongodb':
      return 27017;
    case 'redis':
    case 'keydb':
    case 'dragonfly':
      return 6379;
    case 'clickhouse':
      return 9000;
    default:
      return null;
  }
}

@Component({
  selector: 'app-application-detail',
  standalone: true,
  imports: [
    StatusBadgeComponent,
    IconComponent,
    CardComponent,
    RouterLink,
    ApplicationSettingsTabComponent,
    ApplicationEnvsTabComponent,
    ApplicationStoragesTabComponent,
    ApplicationTasksTabComponent,
    ApplicationDeploymentsTabComponent,
    ApplicationWebhookTabComponent,
    ApplicationDangerTabComponent,
    ApplicationPreviewsTabComponent,
    ApplicationTerminalTabComponent,
    ApplicationLogsTabComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <header class="akd-bar head">
      <a
        routerLink="/applications"
        class="akd-iconbtn akd-iconbtn--bordered"
        aria-label="Back to applications"
      >
        <akd-icon name="arrow-left" [size]="15" />
      </a>
      <h1 class="name">{{ application()?.name ?? '…' }}</h1>
      @if (application(); as app) {
        <akd-status-badge
          domain="resource"
          [state]="app.desired_status"
          [label]="'desired: ' + app.desired_status"
        />
        <akd-status-badge
          domain="resource"
          [state]="app.observed_status"
          [label]="'observed: ' + app.observed_status"
        />
        @if (app.domains?.[0]; as domain) {
          <a class="akd-mono" [href]="'https://' + domain" target="_blank" rel="noopener">
            {{ domain }}
          </a>
        }
        <span class="spacer"></span>
        @if (app.git_branch; as branch) {
          <span class="akd-badge akd-badge--mono">{{ branch }}</span>
        }
        @if (app.build_pack; as pack) {
          <span class="akd-badge akd-badge--mono">{{ pack }}</span>
        }
        @if (serverName(); as server) {
          <span class="akd-badge akd-badge--mono">{{ server }}</span>
        }
        <div class="actions">
          <button
            class="akd-btn akd-btn--secondary"
            type="button"
            [disabled]="busy()"
            (click)="run('restart')"
          >
            <akd-icon name="refresh-cw" [size]="15" />
            Restart
          </button>
          @if (app.desired_status === 'stopped') {
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              [disabled]="busy()"
              (click)="run('start')"
            >
              <akd-icon name="play" [size]="15" />
              Start
            </button>
          } @else {
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              [disabled]="busy()"
              (click)="run('stop')"
            >
              <akd-icon name="square" [size]="13" />
              Stop
            </button>
          }
          <button
            class="akd-btn akd-btn--primary"
            type="button"
            [disabled]="busy()"
            (click)="run('deploy')"
          >
            <akd-icon name="rocket" [size]="15" />
            Deploy
          </button>
        </div>
      }
    </header>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <nav class="akd-tabs" role="tablist" aria-label="Application sections">
      @for (t of tabs; track t.id) {
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="tab() === t.id"
          [attr.aria-selected]="tab() === t.id"
          (click)="selectTab(t.id)"
        >
          {{ t.label }}
          @if (t.id === 'deployments' && deployments().length > 0) {
            <span class="akd-tab__count">{{ deployments().length }}</span>
          }
        </button>
      }
    </nav>

    @switch (tab()) {
      @case ('overview') {
        @if (application(); as app) {
          <section class="cards">
            <div class="akd-card">
              <!-- Intent and observation are shown side by side and never merged: a
                   desired "running" says nothing about what is actually up (§19.2). -->
              <span class="akd-stat__label">Desired</span>
              <span><akd-status-badge domain="resource" [state]="app.desired_status" /></span>
            </div>
            <div class="akd-card">
              <span class="akd-stat__label">Observed</span>
              <span><akd-status-badge domain="resource" [state]="app.observed_status" /></span>
            </div>
            <div class="akd-card">
              <span class="akd-stat__label">Source</span>
              <span class="akd-mono">{{ app.source_type }}</span>
            </div>
            <div class="akd-card">
              <span class="akd-stat__label">Domains</span>
              @if (app.domains?.length) {
                <ul class="domains akd-mono">
                  @for (domain of app.domains; track domain) {
                    <li>{{ domain }}</li>
                  }
                </ul>
              } @else {
                <span class="akd-muted">None</span>
              }
            </div>
          </section>

          @if (components().length > 0) {
            <akd-card title="Stack components" class="components" [padded]="false">
              <div class="comp">
                <!-- One tab per compose service: its state and the actions that
                     apply to it (logs, shell, tunnel) live in its own panel,
                     instead of a flat list the operator has to cross-reference. -->
                <nav class="comp__tabs" role="tablist" aria-label="Stack components">
                  @for (c of components(); track c.uuid) {
                    <button
                      type="button"
                      role="tab"
                      class="comp__tab"
                      [class.comp__tab--active]="activeComponent() === c.name"
                      [attr.aria-selected]="activeComponent() === c.name"
                      (click)="activeComponent.set(c.name)"
                    >
                      <span class="akd-mono comp__tab-name">{{ c.name }}</span>
                      <akd-status-badge domain="resource" [state]="c.observed_status" />
                    </button>
                  }
                </nav>

                @if (activeComp(); as c) {
                  <div class="comp__panel" role="tabpanel">
                    <header class="comp__head">
                      <span class="akd-mono comp__title">{{ c.name }}</span>
                      <akd-status-badge domain="resource" [state]="c.observed_status" />
                      @if (c.is_database) {
                        <span class="akd-badge akd-badge--mono">db: {{ c.database_engine }}</span>
                      }
                      @if (c.exclude_from_hc) {
                        <span class="akd-badge">one-shot</span>
                      }
                    </header>

                    @if (c.image || c.observed_at) {
                      <dl class="comp__meta">
                        @if (c.image) {
                          <div>
                            <dt>Image</dt>
                            <dd class="akd-mono">{{ c.image }}</dd>
                          </div>
                        }
                        @if (c.observed_at) {
                          <div>
                            <dt>Last seen</dt>
                            <dd>{{ c.observed_at }}</dd>
                          </div>
                        }
                      </dl>
                    }

                    <div class="comp__actions">
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm"
                        type="button"
                        (click)="openComponent('logs', c.name)"
                      >
                        <akd-icon name="scroll-text" [size]="13" />
                        Logs
                      </button>
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm"
                        type="button"
                        (click)="openComponent('terminal', c.name)"
                      >
                        <akd-icon name="terminal" [size]="13" />
                        Shell
                      </button>
                      @if (c.is_database) {
                        <button
                          class="akd-btn akd-btn--secondary akd-btn--sm"
                          type="button"
                          (click)="selectTab('storages')"
                        >
                          <akd-icon name="hard-drive" [size]="13" />
                          Volumes
                        </button>
                      }
                    </div>

                    <!-- Reach this container from your machine without exposing it
                         (CLI tunnels through the manager — cli.md §7). -->
                    <div class="comp__cli">
                      @if (c.is_database) {
                        <span class="comp__cli-label">Open a console</span>
                        <div class="comp__cmd">
                          <code class="akd-mono">{{ dbConsoleCmd(c) }}</code>
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            title="Copy"
                            (click)="copy(dbConsoleCmd(c))"
                          >
                            <akd-icon name="copy" [size]="13" />
                          </button>
                        </div>
                      }
                      <span class="comp__cli-label">Forward a port</span>
                      <div class="comp__cmd">
                        <code class="akd-mono">{{ portForwardCmd(c) }}</code>
                        <button
                          class="akd-btn akd-btn--ghost akd-btn--sm"
                          type="button"
                          title="Copy"
                          (click)="copy(portForwardCmd(c))"
                        >
                          <akd-icon name="copy" [size]="13" />
                        </button>
                      </div>
                      @if (notice(); as text) {
                        <span class="comp__notice">{{ text }}</span>
                      }
                    </div>
                  </div>
                }
              </div>
            </akd-card>
          }
        }

        <section class="split">
          <akd-card title="Deployments" [padded]="false">
            @if (deployments().length === 0) {
              <p class="akd-muted pad">No deployment yet.</p>
            } @else {
              <ul class="deploy-list">
                @for (d of deployments(); track d.uuid) {
                  <li [class.selected]="d.uuid === selected()">
                    <button type="button" class="row" (click)="select(d.uuid!)">
                      <akd-status-badge domain="deployment" [state]="d.status" />
                      <span class="trigger"
                        >{{ d.trigger }}{{ d.is_rollback ? ' · rollback' : '' }}</span
                      >
                      <span class="akd-muted when">{{ d.created_at }}</span>
                    </button>
                  </li>
                }
              </ul>
            }
          </akd-card>

          <akd-card title="Build logs" [padded]="false">
            <span card-actions>
              @if (streaming()) {
                <akd-status-badge domain="job" state="running" label="live · SSE" />
              }
            </span>
            @if (!selected()) {
              <p class="akd-muted pad">Pick a deployment to read its logs.</p>
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
        </section>
      }
      @case ('settings') {
        <app-application-settings-tab [uuid]="uuid()" (saved)="onSettingsSaved($event)" />
      }
      @case ('envs') {
        <app-application-envs-tab [uuid]="uuid()" />
      }
      @case ('storages') {
        <app-application-storages-tab [uuid]="uuid()" />
      }
      @case ('tasks') {
        <app-application-tasks-tab [uuid]="uuid()" />
      }
      @case ('deployments') {
        <app-application-deployments-tab [uuid]="uuid()" />
      }
      @case ('previews') {
        <app-application-previews-tab [uuid]="uuid()" />
      }
      @case ('logs') {
        <app-application-logs-tab [uuid]="uuid()" [preselect]="componentParam() ?? ''" />
      }
      @case ('terminal') {
        <app-application-terminal-tab [uuid]="uuid()" [preselect]="componentParam() ?? ''" />
      }
      @case ('webhook') {
        <app-application-webhook-tab [uuid]="uuid()" />
      }
      @case ('danger') {
        <app-application-danger-tab [uuid]="uuid()" [name]="application()?.name ?? ''" />
      }
    }
  `,
  styles: [
    `
      /* Tokens only — no literal colour or size (design-system §6.1). */
      .head {
        justify-content: flex-start;
        row-gap: var(--space-2);
      }
      h1.name {
        font-family: var(--font-mono);
      }
      .spacer {
        flex: 1;
      }
      .actions {
        display: flex;
        gap: var(--space-2);
      }
      .cards {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
        gap: var(--space-3);
        margin-bottom: var(--space-5);
      }
      .components {
        display: block;
        margin-bottom: var(--space-5);
      }
      /* Left rail of services, right panel for the selected one; stacks on
         narrow screens (the rail becomes a horizontal strip). */
      .comp {
        display: grid;
        grid-template-columns: minmax(11rem, 15rem) 1fr;
        align-items: stretch;
      }
      .comp__tabs {
        display: flex;
        flex-direction: column;
        gap: var(--space-1);
        padding: var(--space-3);
        border-right: 1px solid var(--border-1);
      }
      .comp__tab {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        width: 100%;
        padding: var(--space-2) var(--space-3);
        border: 0;
        border-radius: var(--radius-2);
        background: transparent;
        color: var(--text-1);
        cursor: pointer;
        text-align: left;
        font: inherit;
      }
      .comp__tab:hover {
        background: var(--bg-2);
      }
      .comp__tab:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .comp__tab--active {
        background: var(--bg-2);
      }
      .comp__tab-name {
        flex: 1;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        font-size: var(--text-sm);
      }
      .comp__panel {
        padding: var(--space-4) var(--space-5);
        display: grid;
        gap: var(--space-4);
        align-content: start;
      }
      .comp__head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .comp__title {
        font-size: var(--text-base);
      }
      .comp__meta {
        display: grid;
        gap: var(--space-2);
        margin: 0;
        font-size: var(--text-sm);
      }
      .comp__meta > div {
        display: flex;
        gap: var(--space-3);
      }
      .comp__meta dt {
        min-width: 5.5rem;
        color: var(--text-3);
      }
      .comp__meta dd {
        margin: 0;
        overflow-wrap: anywhere;
      }
      .comp__actions {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-2);
      }
      .comp__cli {
        display: grid;
        gap: var(--space-2);
        padding-top: var(--space-3);
        border-top: 1px solid var(--border-1);
      }
      .comp__cli-label {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .comp__cmd {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .comp__cmd code {
        flex: 1;
        padding: var(--space-2) var(--space-3);
        border-radius: var(--radius-2);
        background: var(--surface-2);
        font-size: var(--text-xs);
        overflow-x: auto;
        white-space: nowrap;
      }
      .comp__notice {
        font-size: var(--text-xs);
        color: var(--text-success, var(--text-2));
      }
      @media (max-width: 48rem) {
        .comp {
          grid-template-columns: 1fr;
        }
        .comp__tabs {
          flex-direction: row;
          flex-wrap: wrap;
          border-right: 0;
          border-bottom: 1px solid var(--border-1);
        }
        .comp__tab {
          width: auto;
        }
        .comp__tab-name {
          flex: 0 1 auto;
        }
      }
      .split {
        display: grid;
        grid-template-columns: minmax(16rem, 24rem) 1fr;
        gap: var(--space-5);
        align-items: start;
      }
      .deploy-list {
        list-style: none;
        margin: 0;
        padding: 0;
      }
      .deploy-list li + li {
        border-top: 1px solid var(--border-1);
      }
      .deploy-list .selected {
        background: var(--bg-2);
      }
      .row {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        width: 100%;
        padding: var(--space-2) var(--space-3);
        font: inherit;
        text-align: left;
        background: transparent;
        border: 0;
        cursor: pointer;
        color: var(--text-1);
      }
      .row:hover {
        background: var(--bg-2);
      }
      .row:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .trigger {
        font-size: var(--text-sm);
      }
      .when {
        margin-left: auto;
        font-size: var(--text-xs);
      }
      .pad {
        margin: 0;
        padding: var(--space-4) var(--space-5);
      }
      .logpane {
        border: none;
        border-radius: 0 0 var(--radius-3) var(--radius-3);
        max-height: 60vh;
        padding: var(--space-2) 0;
      }
      .logpane:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .domains {
        list-style: none;
        margin: 0;
        padding: 0;
      }
      @media (max-width: 60rem) {
        .split {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class ApplicationDetailComponent {
  /** Bound from the route (`applications/:uuid`) by withComponentInputBinding. */
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly tabs: readonly { id: TabId; label: string }[] = [
    { id: 'overview', label: 'Overview' },
    { id: 'settings', label: 'Settings' },
    { id: 'envs', label: 'Environment variables' },
    { id: 'storages', label: 'Storages' },
    { id: 'tasks', label: 'Scheduled tasks' },
    { id: 'deployments', label: 'Deployments' },
    { id: 'logs', label: 'Logs' },
    { id: 'previews', label: 'Previews' },
    { id: 'terminal', label: 'Terminal' },
    { id: 'webhook', label: 'Webhook' },
    { id: 'danger', label: 'Danger' },
  ];
  protected readonly tab = signal<TabId>('overview');
  /** The active tab lives in the URL (?tab=…): a refresh keeps it, and
   * back/forward walk the tabs — withComponentInputBinding feeds this input
   * from the query parameter on every navigation. */
  readonly tabParam = input<string | undefined>(undefined, { alias: 'tab' });
  /** Deep-link into the logs/terminal tabs with a compose service preselected. */
  readonly componentParam = input<string | undefined>(undefined, { alias: 'component' });

  private readonly router = inject(Router);
  private readonly activatedRoute = inject(ActivatedRoute);

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      queryParams: { tab: id === 'overview' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  /** Jump to the logs/terminal tab with this compose service preselected. */
  protected openComponent(tab: 'logs' | 'terminal', component: string): void {
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      queryParams: { tab, component },
      queryParamsHandling: 'merge',
    });
  }

  protected readonly application = signal<Application | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  /** Which stack component's panel is open in the overview. */
  protected readonly activeComponent = signal<string | null>(null);
  protected readonly activeComp = computed(
    () => this.components().find((c) => c.name === this.activeComponent()) ?? null,
  );
  /** Transient "copied" feedback under the CLI commands. */
  protected readonly notice = signal<string | null>(null);
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;
  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly serverName = signal<string | null>(null);
  protected readonly selected = signal<string | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly streaming = signal(false);

  /**
   * The lines, keyed by sequence. The sequence IS the identity of a line
   * (§27.24), so a reconnect that replays what we already have overwrites it
   * instead of duplicating it — which is what makes Last-Event-ID resumption
   * safe to rely on without a de-dup pass at the protocol level.
   */
  private readonly lines = signal<Map<number, Row>>(new Map());
  protected readonly rows = computed(() =>
    [...this.lines().values()].sort((a, b) => a.sequence - b.sequence),
  );

  private source: EventSource | null = null;

  constructor() {
    // URL → state: seeds the tab on load and follows back/forward — the
    // navigation history is the source of truth for which tab is open.
    effect(() => {
      const wanted = this.tabParam();
      const valid = this.tabs.find((t) => t.id === wanted)?.id;
      this.tab.set(valid ?? this.tabs[0].id);
    });
    inject(DestroyRef).onDestroy(() => {
      this.closeStream();
      if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    });
    // The uuid is a route input: it is not readable before the router binds it,
    // so the initial load waits for the effect rather than the constructor.
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected isGap = isGap;

  protected render(row: Row): string {
    if (isGap(row))
      return '⚠ lines dropped by the server (backpressure) — the log is incomplete here';
    const prefix = row.channel === 'stderr' ? '! ' : row.channel === 'system' ? '· ' : '  ';
    return prefix + row.message;
  }

  /** HH:MM:SS from an RFC 3339 timestamp — the date is in the list next door. */
  protected clock(timestamp: string): string {
    return timestamp.slice(11, 19);
  }

  private async load(uuid: string): Promise<void> {
    const client = this.api.client();
    try {
      const [app, page, comps] = await Promise.all([
        client.getApplication(uuid),
        client.listApplicationDeployments(uuid, { limit: 20 }),
        client.listApplicationComponents(uuid),
      ]);
      this.application.set(app);
      this.setComponents(comps.data);
      this.deployments.set(page.data);
      void this.loadServerName(app.server_uuid);
      // The newest deployment is the one an operator is here to watch.
      const latest = page.data[0]?.uuid;
      if (latest && !this.selected()) this.select(latest);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /** Header badge only — its absence must never block the page. */
  private async loadServerName(serverUuid: string): Promise<void> {
    try {
      const server = await this.api.client().getServer(serverUuid);
      this.serverName.set(server.name);
    } catch {
      this.serverName.set(null);
    }
  }

  /** The settings tab saved a new version: reflect it (name, domains, version). */
  protected onSettingsSaved(app: Application): void {
    this.application.set(app);
  }

  protected select(deploymentUuid: string): void {
    if (this.selected() === deploymentUuid) return;
    this.selected.set(deploymentUuid);
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
    // on purpose, so close it rather than let EventSource reconnect forever.
    source.addEventListener('end', () => {
      this.closeStream();
      void this.refreshDeployments();
    });
  }

  private closeStream(): void {
    this.source?.close();
    this.source = null;
    this.streaming.set(false);
  }

  private async refreshDeployments(): Promise<void> {
    try {
      const [app, page, comps] = await Promise.all([
        this.api.client().getApplication(this.uuid()),
        this.api.client().listApplicationDeployments(this.uuid(), { limit: 20 }),
        this.api.client().listApplicationComponents(this.uuid()),
      ]);
      this.application.set(app);
      this.setComponents(comps.data);
      this.deployments.set(page.data);
    } catch {
      // A failed refresh must not wipe what is already on screen.
    }
  }

  /** Sets the components and keeps the open panel valid (or opens the first). */
  private setComponents(comps: ServiceComponent[]): void {
    this.components.set(comps);
    const current = this.activeComponent();
    if (!current || !comps.some((c) => c.name === current)) {
      this.activeComponent.set(comps[0]?.name ?? null);
    }
  }

  /** CLI reference of this app: `app/<name>` (falls back to the UUID). */
  private appRef(): string {
    return 'app/' + (this.application()?.name ?? this.uuid());
  }

  /** Confort console for a database service (cli.md §8). */
  protected dbConsoleCmd(c: ServiceComponent): string {
    return `akerdock db ${this.appRef()} -c ${c.name}`;
  }

  /** TCP tunnel through the manager to this service (cli.md §7). */
  protected portForwardCmd(c: ServiceComponent): string {
    const port = enginePort(c.database_engine) ?? '<PORT>';
    return `akerdock port-forward ${this.appRef()} ${port}:${port} -c ${c.name}`;
  }

  protected async copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      this.flashNotice('Copied to clipboard.');
    } catch {
      this.flashNotice('Copy failed — select and copy manually.');
    }
  }

  private flashNotice(text: string): void {
    this.notice.set(text);
    if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => this.notice.set(null), 2500);
  }

  /**
   * Every action here is asynchronous by contract (202 + job): the UI enqueues
   * and then observes. It never blocks on the outcome, and closing the page
   * never cancels the work.
   */
  protected async run(action: 'deploy' | 'start' | 'stop' | 'restart'): Promise<void> {
    const client = this.api.client();
    this.busy.set(true);
    this.error.set(null);
    try {
      switch (action) {
        case 'deploy': {
          const { deployment_uuid } = await client.deployApplication(this.uuid());
          await this.refreshDeployments();
          // Follow the deployment that was just queued: watching it is the whole
          // point of having asked for it.
          if (deployment_uuid) {
            this.tab.set('overview');
            this.select(deployment_uuid);
          }
          break;
        }
        case 'start':
          await client.startApplication(this.uuid());
          break;
        case 'stop':
          await client.stopApplication(this.uuid());
          break;
        case 'restart':
          await client.restartApplication(this.uuid());
          break;
      }
      if (action !== 'deploy') await this.refreshDeployments();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
