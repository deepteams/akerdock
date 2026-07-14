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
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
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
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <header class="bar">
      <div>
        <a routerLink="/applications" class="back">← Applications</a>
        <h1>{{ application()?.name ?? '…' }}</h1>
      </div>
      @if (application(); as app) {
        <div class="actions">
          <button class="ghost" type="button" [disabled]="busy()" (click)="run('deploy')">
            Deploy
          </button>
          <button class="ghost" type="button" [disabled]="busy()" (click)="run('restart')">
            Restart
          </button>
          @if (app.desired_status === 'stopped') {
            <button class="ghost" type="button" [disabled]="busy()" (click)="run('start')">
              Start
            </button>
          } @else {
            <button class="ghost" type="button" [disabled]="busy()" (click)="run('stop')">
              Stop
            </button>
          }
        </div>
      }
    </header>

    @if (error(); as message) {
      <p class="error" role="alert">{{ message }}</p>
    }

    <nav class="akd-tabs" role="tablist" aria-label="Application sections">
      @for (t of tabs; track t.id) {
        <button
          type="button"
          role="tab"
          [attr.aria-selected]="tab() === t.id"
          (click)="tab.set(t.id)"
        >
          {{ t.label }}
        </button>
      }
    </nav>

    @switch (tab()) {
      @case ('overview') {
        @if (application(); as app) {
          <section class="cards">
            <div class="card">
              <h2>Desired</h2>
              <!-- Intent and observation are shown side by side and never merged: a
                   desired "running" says nothing about what is actually up (§19.2). -->
              <akd-status-badge domain="resource" [state]="app.desired_status" />
            </div>
            <div class="card">
              <h2>Observed</h2>
              <akd-status-badge domain="resource" [state]="app.observed_status" />
            </div>
            <div class="card">
              <h2>Source</h2>
              <p class="muted">{{ app.source_type }}</p>
            </div>
            <div class="card">
              <h2>Domains</h2>
              @if (app.domains?.length) {
                <ul class="domains">
                  @for (domain of app.domains; track domain) {
                    <li>{{ domain }}</li>
                  }
                </ul>
              } @else {
                <p class="muted">None</p>
              }
            </div>
          </section>

          @if (components().length > 0) {
            <section class="components">
              <h2>Stack components</h2>
              <ul class="component-list">
                @for (c of components(); track c.uuid) {
                  <li>
                    <span class="akd-mono">{{ c.name }}</span>
                    @if (c.is_database) {
                      <span class="muted">db: {{ c.database_engine }}</span>
                    }
                    @if (c.exclude_from_hc) {
                      <span class="muted">one-shot</span>
                    }
                    <akd-status-badge domain="resource" [state]="c.observed_status" />
                  </li>
                }
              </ul>
            </section>
          }
        }

        <section class="split">
          <div>
            <h2>Deployments</h2>
            @if (deployments().length === 0) {
              <p class="muted">No deployment yet.</p>
            } @else {
              <ul class="timeline">
                @for (d of deployments(); track d.uuid) {
                  <li [class.selected]="d.uuid === selected()">
                    <button type="button" class="row" (click)="select(d.uuid!)">
                      <akd-status-badge domain="deployment" [state]="d.status" />
                      <span class="trigger"
                        >{{ d.trigger }}{{ d.is_rollback ? ' · rollback' : '' }}</span
                      >
                      <span class="muted when">{{ d.created_at }}</span>
                    </button>
                  </li>
                }
              </ul>
            }
          </div>

          <div class="logs-pane">
            <h2>
              Build logs
              @if (streaming()) {
                <span class="live" role="status">live</span>
              }
            </h2>
            @if (!selected()) {
              <p class="muted">Pick a deployment to read its logs.</p>
            } @else {
              <pre
                class="logs"
                tabindex="0"
                aria-label="Deployment build logs"
              >@for (row of rows(); track row.sequence) {<span
                  class="line"
                  [class.stderr]="!isGap(row) && row.channel === 'stderr'"
                  [class.system]="!isGap(row) && row.channel === 'system'"
                  [class.gap]="isGap(row)"
                >{{ render(row) }}</span>}</pre>
            }
          </div>
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
      :host {
        display: block;
        padding: var(--akd-space-6);
        background: var(--akd-bg);
        min-height: 100vh;
      }
      .bar {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: var(--akd-space-5);
      }
      .back {
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
      }
      .back:hover {
        text-decoration: underline;
      }
      h1 {
        margin: var(--akd-space-1) 0 0;
        font-size: var(--akd-text-xl);
        color: var(--akd-text);
      }
      h2 {
        margin: 0 0 var(--akd-space-2);
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.04em;
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .components {
        margin-bottom: var(--akd-space-6);
      }
      .component-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: grid;
        gap: var(--akd-space-1);
        font-size: var(--akd-text-sm);
      }
      .component-list li {
        display: flex;
        align-items: center;
        gap: var(--akd-space-3);
        padding: var(--akd-space-1) 0;
      }
      .cards {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
        gap: var(--akd-space-3);
        margin-bottom: var(--akd-space-6);
      }
      .card {
        padding: var(--akd-space-3);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        background: var(--akd-surface);
      }
      .split {
        display: grid;
        grid-template-columns: minmax(16rem, 22rem) 1fr;
        gap: var(--akd-space-5);
        align-items: start;
      }
      .timeline {
        list-style: none;
        margin: 0;
        padding: 0;
      }
      .timeline li + li {
        border-top: 1px solid var(--akd-border);
      }
      .timeline .selected {
        background: var(--akd-surface-hover);
      }
      .row {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        width: 100%;
        padding: var(--akd-space-2);
        font: inherit;
        text-align: left;
        background: transparent;
        border: 0;
        cursor: pointer;
        color: var(--akd-text);
      }
      .row:hover {
        background: var(--akd-surface-hover);
      }
      .row:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: -2px;
      }
      .trigger {
        font-size: var(--akd-text-sm);
      }
      .when {
        margin-left: auto;
        font-size: var(--akd-text-xs);
      }
      .live {
        margin-left: var(--akd-space-2);
        color: var(--akd-status-progress-fg);
        text-transform: none;
        letter-spacing: 0;
      }
      .logs {
        margin: 0;
        padding: var(--akd-space-3);
        max-height: 60vh;
        overflow: auto;
        background: var(--akd-surface);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        font-family: var(--akd-font-mono);
        font-size: var(--akd-text-xs);
        line-height: 1.6;
        color: var(--akd-text);
        white-space: pre-wrap;
        word-break: break-word;
      }
      .logs:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
      }
      .line {
        display: block;
      }
      /* stderr and system are marked by a prefix in the text as well as by
         colour: a log read in black and white must still be readable. */
      .line.stderr {
        color: var(--akd-status-danger-fg);
      }
      .line.system {
        color: var(--akd-text-secondary);
      }
      .line.gap {
        color: var(--akd-status-warning-fg);
        font-weight: var(--akd-weight-semibold);
      }
      .sr-only {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
        border: 0;
      }
      .domains {
        list-style: none;
        margin: 0;
        padding: 0;
        font-size: var(--akd-text-sm);
      }
      .muted {
        color: var(--akd-text-secondary);
        margin: 0;
      }
      .ghost {
        padding: var(--akd-space-1) var(--akd-space-3);
        font: inherit;
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
        background: transparent;
        border: 1px solid var(--akd-border-input);
        border-radius: var(--akd-radius-sm);
        cursor: pointer;
      }
      .ghost:hover:not(:disabled) {
        background: var(--akd-surface-hover);
      }
      .ghost:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      .ghost:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: 1px;
      }
      .error {
        padding: var(--akd-space-2) var(--akd-space-3);
        margin-bottom: var(--akd-space-4);
        color: var(--akd-status-danger-fg);
        background: var(--akd-status-danger-bg);
        border-radius: var(--akd-radius-sm);
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
    { id: 'previews', label: 'Previews' },
    { id: 'terminal', label: 'Terminal' },
    { id: 'webhook', label: 'Webhook' },
    { id: 'danger', label: 'Danger' },
  ];
  protected readonly tab = signal<TabId>('overview');

  protected readonly application = signal<Application | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  protected readonly deployments = signal<Deployment[]>([]);
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
      // The newest deployment is the one an operator is here to watch.
      const latest = page.data[0]?.uuid;
      if (latest && !this.selected()) this.select(latest);
    } catch (err) {
      this.error.set(ApiService.describe(err));
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
