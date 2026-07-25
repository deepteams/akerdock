import {
  ChangeDetectionStrategy,
  Component,
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
import {
  StackComponentsComponent,
  type StackComponentAction,
} from '../../ui/stack-components/stack-components.component';
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
    CardComponent,
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
            <akd-stack-components
              class="components"
              [components]="components()"
              [appName]="app.name"
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
      queryParams: { tab: action.target, component: action.component },
      queryParamsHandling: 'merge',
    });
  }

  protected readonly application = signal<Application | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly serverName = signal<string | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

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
  protected async run(action: 'deploy' | 'start' | 'stop' | 'restart'): Promise<void> {
    const client = this.api.client();
    this.busy.set(true);
    this.error.set(null);
    try {
      switch (action) {
        case 'deploy': {
          const { deployment_uuid } = await client.deployApplication(this.uuid());
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
