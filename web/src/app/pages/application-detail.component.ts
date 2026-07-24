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
            <akd-card title="Stack components" class="components">
              <ul class="component-list">
                @for (c of components(); track c.uuid) {
                  <li>
                    <span class="akd-mono">{{ c.name }}</span>
                    @if (c.is_database) {
                      <span class="akd-badge akd-badge--mono">db: {{ c.database_engine }}</span>
                    }
                    @if (c.exclude_from_hc) {
                      <span class="akd-badge">one-shot</span>
                    }
                    <akd-status-badge domain="resource" [state]="c.observed_status" />
                  </li>
                }
              </ul>
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
        <app-application-logs-tab [uuid]="uuid()" />
      }
      @case ('terminal') {
        <app-application-terminal-tab [uuid]="uuid()" />
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
      .component-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: grid;
        gap: var(--space-1);
        font-size: var(--text-sm);
      }
      .component-list li {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-1) 0;
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

  protected readonly application = signal<Application | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
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
    inject(DestroyRef).onDestroy(() => this.closeStream());
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
      this.components.set(comps.data);
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
      this.components.set(comps.data);
      this.deployments.set(page.data);
    } catch {
      // A failed refresh must not wipe what is already on screen.
    }
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
