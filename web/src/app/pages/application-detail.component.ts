import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { ActivatedRoute, Router, RouterLink, UrlTree } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { IconComponent } from '../../ui/icon/icon.component';
import {
  ActionsMenuComponent,
  type ActionItem,
} from '../../ui/actions-menu/actions-menu.component';
import {
  StackComponentsComponent,
  type StackComponentAction,
} from '../../ui/stack-components/stack-components.component';
import { ApiService } from '../core/api.service';
import { NavigationHistory } from '../core/navigation-history.service';
import { APPLICATION_EVENTS, DEPLOYMENT_EVENTS, LiveEventsService } from '../core/live-refresh';
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
type ComponentMetric = components['schemas']['ComponentMetric'];

/**
 * `recreate` is the one an operator reaches for after editing a variable: a
 * container freezes its environment when it is created, so `restart` hands
 * the process back the values it already had (ADR-048).
 */
type AppAction = 'deploy' | 'rebuild' | 'recreate' | 'start' | 'stop' | 'restart';

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

@Component({
  selector: 'app-application-detail',
  standalone: true,
  imports: [
    StatusBadgeComponent,
    IconComponent,
    ActionsMenuComponent,
    StackComponentsComponent,
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
      <a [routerLink]="backLink()" class="akd-iconbtn akd-iconbtn--bordered" aria-label="Back">
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
          <akd-actions-menu
            [items]="actions()"
            [disabled]="busy()"
            (selected)="run($any($event))"
          />
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

          @if (app.build_pack === 'compose') {
            @if (components().length > 0) {
              <akd-stack-components
                class="components"
                [components]="components()"
                [appName]="app.name"
                [metrics]="metrics()"
                (open)="onComponentAction($event)"
              />
            }
          } @else {
            <!-- Single-container build pack: one instance panel (state, logs,
                 shell, port-forward, live stats) — same helpers as a stack. -->
            <akd-stack-components
              class="components"
              [single]="true"
              [components]="singleComponent()"
              [appName]="app.name"
              [metrics]="metrics()"
              (open)="onComponentAction($event)"
            />
          }
        }
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
        <app-application-previews-tab
          [uuid]="uuid()"
          [deployOnOpen]="application()?.preview_deploy_on_open"
        />
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
      .domains {
        list-style: none;
        margin: 0;
        padding: 0;
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
  private readonly history = inject(NavigationHistory);

  /** Back where the user came from — the environment's resource table as often
   *  as the flat list, which is why this is not a fixed link. */
  protected backLink(): UrlTree {
    return this.history.backTo('/applications');
  }

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      queryParams: { tab: id === 'overview' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  /** A stack-components action: deep-link to that service's logs/shell/volumes. */
  protected onComponentAction(action: StackComponentAction): void {
    if (action.target === 'storages') {
      this.selectTab('storages');
      return;
    }
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      // A single container has no compose service: omit the component param.
      queryParams: { tab: action.target, component: action.component || null },
      queryParamsHandling: 'merge',
    });
  }

  protected readonly application = signal<Application | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly serverName = signal<string | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  /** Live per-service stats (ADR-034), keyed by component name — polled only
   * while the overview tab of a compose stack is open. */
  protected readonly metrics = signal<Record<string, ComponentMetric>>({});

  /** Whether the app is a single-container build pack (no compose services). */
  private readonly single = computed(() => this.application()?.build_pack !== 'compose');

  /**
   * The overflow menu. Each hint names the consequence, because the entries
   * differ by one word and by a great deal of effect: `restart` reuses the
   * container as it stands, `recreate` builds it again from the current
   * configuration — the only one of the two that picks up an edited variable
   * (ADR-048).
   */
  protected readonly actions = computed<ActionItem[]>(() => {
    const app = this.application();
    const items: ActionItem[] = [
      {
        id: 'deploy',
        label: 'Deploy',
        icon: 'rocket',
        hint: 'Build and deploy the application with the current configuration',
      },
    ];
    // Nothing to force a cache past when nothing is built: the contract only
    // names a build pack for the sources that build one (a ready image pulled
    // from a registry reports none).
    if (app?.build_pack) {
      items.push({
        id: 'rebuild',
        label: 'Rebuild (no cache)',
        icon: 'hammer',
        hint: 'Build the image again, ignoring every cached layer',
      });
    }
    items.push({
      id: 'recreate',
      label: 'Recreate (apply config)',
      icon: 'settings-2',
      hint: 'Redeploy the running image with the current variables — no build',
    });
    items.push({
      id: 'restart',
      label: 'Restart',
      icon: 'refresh-cw',
      hint: 'Restart the container as it stands — keeps its current variables',
    });
    if (app?.desired_status === 'stopped') {
      items.push({ id: 'start', label: 'Start', icon: 'play', hint: 'Start the container again' });
    } else {
      items.push({
        id: 'stop',
        label: 'Stop',
        icon: 'square',
        danger: true,
        hint: 'Stop the container — the application goes offline',
      });
    }
    return items;
  });

  /** The synthesized one-entry list that drives the single-container panel. */
  protected readonly singleComponent = computed<ServiceComponent[]>(() => {
    const app = this.application();
    if (!app || !this.single()) return [];
    return [
      {
        uuid: app.uuid,
        name: app.name,
        observed_status: app.observed_status,
        is_database: false,
        exclude_from_hc: false,
        created_at: app.created_at,
      } as unknown as ServiceComponent,
    ];
  });

  constructor() {
    // URL → state: seeds the tab on load and follows back/forward — the
    // navigation history is the source of truth for which tab is open.
    effect(() => {
      const wanted = this.tabParam();
      const valid = this.tabs.find((t) => t.id === wanted)?.id;
      this.tab.set(valid ?? this.tabs[0].id);
    });
    // The uuid is a route input: it is not readable before the router binds it,
    // so the initial load waits for the effect rather than the constructor.
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
    // Live refresh (ADR-024/040): deployments, sleep/wake and the observed
    // component states (pushed by the server agent) move on their own — on
    // the app's ONE shared stream (LiveEventsService).
    const live = inject(LiveEventsService);
    effect((onCleanup) => {
      const uuid = this.uuid();
      onCleanup(
        live.refresh(
          [...APPLICATION_EVENTS, ...DEPLOYMENT_EVENTS],
          (ev) => ev.resource_uuid === uuid,
          () => untracked(() => void this.load(uuid)),
        ),
      );
    });
    // Live metrics: poll only while the overview shows an instance panel — a
    // compose stack with components, or a single-container app — and stop
    // (releasing the SSH round-trip) as soon as it is not.
    effect((onCleanup) => {
      const uuid = this.uuid();
      const app = this.application();
      const hasPanel = this.single() ? !!app : this.components().length > 0;
      if (this.tab() !== 'overview' || !hasPanel) {
        this.metrics.set({});
        return;
      }
      // For a single container the backend reports an empty component name; key
      // it under the synthesized app name so the panel and sparkline line up.
      const fallback = app?.name ?? 'app';
      let stopped = false;
      const poll = async () => {
        try {
          const page = await this.api.client().getApplicationMetrics(uuid);
          if (!stopped) {
            this.metrics.set(
              Object.fromEntries(page.data.map((m) => [m.component || fallback, m])),
            );
          }
        } catch {
          // Transient (server unreachable) — keep the last snapshot on screen.
        }
      };
      void poll();
      const timer = setInterval(poll, 4000);
      onCleanup(() => {
        stopped = true;
        clearInterval(timer);
      });
    });
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
  protected async run(action: AppAction): Promise<void> {
    const client = this.api.client();
    this.busy.set(true);
    this.error.set(null);
    try {
      switch (action) {
        case 'deploy':
        case 'rebuild':
        case 'recreate': {
          const { deployment_uuid } = await client.deployApplication(this.uuid(), {
            forceRebuild: action === 'rebuild',
            skipBuild: action === 'recreate',
          });
          await this.refreshDeployments();
          // Follow the deployment that was just queued: open its page, where the
          // build log streams live (SSE) — watching it is the whole point of
          // having asked for it.
          if (deployment_uuid) {
            void this.router.navigate([
              '/applications',
              this.uuid(),
              'deployments',
              deployment_uuid,
            ]);
          }
          return;
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
      await this.refreshDeployments();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
