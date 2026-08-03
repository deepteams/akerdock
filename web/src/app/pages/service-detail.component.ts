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
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink, UrlTree } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import {
  ActionsMenuComponent,
  type ActionItem,
} from '../../ui/actions-menu/actions-menu.component';
import { ApiService } from '../core/api.service';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { NavigationHistory } from '../core/navigation-history.service';
import { ServiceEnvsComponent } from './variables/service-envs-tab.component';
import { ApiError } from '../../api/client';
import type { components } from '../../api/schema';

type Service = components['schemas']['Service'];
type ServiceComponent = components['schemas']['ServiceComponent'];
type Deployment = components['schemas']['Deployment'];
type ServiceUpdate = components['schemas']['ServiceUpdate'];

type TabId = 'overview' | 'compose' | 'envs' | 'deployments' | 'settings' | 'danger';

/** Nav order — the same order the sections are switched on below. */
const TABS: readonly { id: TabId; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'compose', label: 'Compose file' },
  { id: 'envs', label: 'Environment variables' },
  { id: 'deployments', label: 'Deployments' },
  { id: 'settings', label: 'Settings' },
  { id: 'danger', label: 'Danger' },
];

/**
 * One compose stack: the file is the source of truth, edited here and
 * validated by the API at every save (compose-spec §11).
 *
 * Same tabbed shape as an application — the file simply plays the part the git
 * source plays there: it is what a deployment applies.
 */
@Component({
  selector: 'app-service-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    CardComponent,
    IconComponent,
    StatusBadgeComponent,
    ActionsMenuComponent,
    ServiceEnvsComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div class="title">
          <a class="akd-iconbtn akd-iconbtn--bordered" [routerLink]="backLink()" aria-label="Back">
            <akd-icon name="arrow-left" [size]="15" />
          </a>
          <span class="title__icon"><akd-icon name="boxes" [size]="17" /></span>
          <h1>{{ service()?.name ?? '…' }}</h1>
        </div>
        @if (service()) {
          <div class="actions">
            <akd-actions-menu
              [items]="actions()"
              [disabled]="busy()"
              (selected)="run($any($event))"
            />
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
      @if (notice(); as message) {
        <p class="akd-muted" role="status">{{ message }}</p>
      }

      @if (service(); as svc) {
        <nav class="akd-tabs" role="tablist" aria-label="Stack sections">
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

        <div class="stack">
          @switch (tab()) {
            @case ('overview') {
              <section class="cards">
                <!-- Intent and observation side by side, never merged: a desired
                     "running" says nothing about what is actually up (§19.2). -->
                <div class="akd-card state">
                  <span class="akd-stat__label">Desired</span>
                  <akd-status-badge domain="resource" [state]="svc.desired_status" />
                </div>
                <div class="akd-card state">
                  <span class="akd-stat__label">Observed</span>
                  <akd-status-badge domain="resource" [state]="svc.observed_status" />
                </div>
              </section>

              @if (components().length > 0) {
                <akd-card title="Components">
                  <ul class="component-list">
                    @for (c of components(); track c.uuid) {
                      <li>
                        <span class="component-name">
                          <akd-icon name="boxes" [size]="15" />
                          <span class="akd-mono">{{ c.name }}</span>
                        </span>
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

              <akd-card title="Overview">
                <dl class="akd-dl">
                  <dt>Last deployment</dt>
                  <dd>{{ svc.last_deployment_at ?? 'never' }}</dd>
                  <dt>Last observed</dt>
                  <dd>{{ svc.observed_at ?? 'never' }}</dd>
                  <dt>Access</dt>
                  <dd>{{ accessLabel() }}</dd>
                  <dt>Description</dt>
                  <dd>{{ svc.description || '—' }}</dd>
                </dl>
              </akd-card>
            }

            @case ('compose') {
              <akd-card title="Compose file">
                <form class="compose-form" (ngSubmit)="saveCompose()">
                  <textarea
                    name="compose"
                    class="akd-input akd-input--mono"
                    rows="18"
                    aria-label="Compose file content"
                    [(ngModel)]="composeContent"
                    [disabled]="busy()"
                  ></textarea>
                  <div class="save-row">
                    <button
                      class="akd-btn akd-btn--primary"
                      type="submit"
                      [disabled]="busy() || !composeContent.trim()"
                    >
                      {{ busy() ? 'Saving…' : 'Save file' }}
                    </button>
                    <span class="akd-muted">
                      Validated on save; applied at the next deployment.
                    </span>
                  </div>
                </form>
              </akd-card>
            }

            @case ('envs') {
              <akd-service-envs [serviceUuid]="uuid()" />
            }

            @case ('deployments') {
              <akd-card title="Deployments" [padded]="false">
                @if (deployments().length === 0) {
                  <p class="akd-muted pad">No deployment yet.</p>
                } @else {
                  <table class="akd-table">
                    <caption class="sr-only">
                      Latest deployments of this stack
                    </caption>
                    <thead>
                      <tr>
                        <th scope="col">Status</th>
                        <th scope="col">Trigger</th>
                        <th scope="col" class="right">When</th>
                      </tr>
                    </thead>
                    <tbody>
                      @for (d of deployments(); track d.uuid) {
                        <tr>
                          <td><akd-status-badge domain="deployment" [state]="d.status" /></td>
                          <td>{{ d.trigger }}</td>
                          <td class="akd-muted right">{{ d.created_at }}</td>
                        </tr>
                      }
                    </tbody>
                  </table>
                }
              </akd-card>
            }

            @case ('settings') {
              <akd-card title="Identity">
                <form class="settings-form" (ngSubmit)="saveSettings()">
                  <label class="akd-field">
                    <span class="akd-field__label">Name</span>
                    <input
                      class="akd-input akd-input--mono"
                      name="serviceName"
                      [(ngModel)]="name"
                      [disabled]="busy()"
                    />
                  </label>
                  <label class="akd-field">
                    <span class="akd-field__label">Description</span>
                    <textarea
                      class="akd-input"
                      name="serviceDescription"
                      rows="3"
                      [(ngModel)]="description"
                      [disabled]="busy()"
                    ></textarea>
                  </label>
                  <label class="akd-check">
                    <input
                      type="checkbox"
                      name="serviceConnectPredefined"
                      [(ngModel)]="connectToPredefinedNetwork"
                      [disabled]="busy()"
                    />
                    Attach every component to the destination's predefined network (§2.1)
                  </label>
                  <div class="save-row">
                    <button
                      class="akd-btn akd-btn--primary"
                      type="submit"
                      [disabled]="busy() || !name.trim()"
                    >
                      {{ busy() ? 'Saving…' : 'Save' }}
                    </button>
                    <span class="akd-muted">Network changes apply at the next deployment.</span>
                  </div>
                </form>
              </akd-card>

              <akd-card title="Access protection">
                <form class="settings-form" (ngSubmit)="saveAccess()">
                  <label class="akd-field">
                    <span class="akd-field__label">Who can reach this stack</span>
                    <select
                      class="akd-input"
                      name="serviceAccessProtection"
                      [(ngModel)]="accessProtection"
                      [disabled]="busy()"
                    >
                      <option value="none">Public (default)</option>
                      <option value="sso">AkerDock login (team members only)</option>
                      <option value="basic_auth">Basic auth (shared credentials)</option>
                    </select>
                  </label>
                  @if (accessProtection === 'basic_auth') {
                    <label class="akd-field">
                      <span class="akd-field__label">
                        Shared credentials, user:password (empty = keep / generate)
                      </span>
                      <input
                        class="akd-input akd-input--mono"
                        name="serviceAccessBasicAuth"
                        autocomplete="off"
                        [(ngModel)]="accessBasicAuth"
                        [disabled]="busy()"
                      />
                    </label>
                  }
                  <p class="akd-muted">
                    The wall covers every routed component. Put
                    <code>x-akerdock.access_public_routes</code> on a Compose service to expose only
                    its webhook or callback paths.
                  </p>
                  <label class="akd-check">
                    <input
                      type="checkbox"
                      name="serviceNoindex"
                      [(ngModel)]="noindex"
                      [disabled]="busy()"
                    />
                    Keep out of search results (<code>X-Robots-Tag: noindex, nofollow</code> on
                    every routed component)
                  </label>
                  <div class="save-row">
                    <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                      {{ busy() ? 'Saving…' : 'Save' }}
                    </button>
                    <span class="akd-muted">The access wall is updated immediately.</span>
                  </div>
                </form>
              </akd-card>
            }

            @case ('danger') {
              <div class="akd-card danger">
                <div class="akd-card__header">
                  <h2 class="akd-card__title">Danger</h2>
                </div>
                <div class="akd-card__body danger-body">
                  <p class="akd-muted">
                    Deletes the routing, every container of the stack and its network. Volumes are
                    kept (INV-008).
                  </p>
                  <button
                    class="akd-btn akd-btn--danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove()"
                  >
                    <akd-icon name="trash-2" [size]="15" />
                    Delete stack
                  </button>
                </div>
              </div>
            }
          }
        </div>
      }
    </div>
  `,
  styles: [
    `
      .title {
        display: flex;
        align-items: center;
        gap: var(--space-3);
      }
      .title__icon {
        color: var(--accent);
        display: inline-flex;
      }
      .actions {
        display: flex;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .stack {
        display: grid;
        gap: var(--space-5);
        max-width: 960px;
      }
      .cards {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
        gap: var(--space-3);
      }
      .state {
        display: grid;
        gap: var(--space-2);
        padding: var(--space-4) var(--space-5);
        justify-items: start;
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
      .component-name {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
        color: var(--text-3);
      }
      .component-name .akd-mono {
        color: var(--text-1);
      }
      .compose-form {
        display: grid;
        gap: var(--space-3);
      }
      .settings-form {
        display: grid;
        gap: var(--space-4);
      }
      .save-row {
        display: flex;
        align-items: center;
        gap: var(--space-3);
      }
      .pad {
        margin: 0;
        padding: var(--space-5);
      }
      .danger {
        border-color: var(--danger-border);
      }
      .danger-body {
        display: grid;
        gap: var(--space-3);
        justify-items: start;
      }
      .danger-body p {
        margin: 0;
      }
    `,
  ],
})
export class ServiceDetailComponent {
  readonly uuid = input.required<string>();
  /** The active tab lives in the URL (?tab=…): a refresh keeps it, and
   * back/forward walk the tabs — withComponentInputBinding feeds this input
   * from the query parameter on every navigation. */
  readonly tabParam = input<string | undefined>(undefined, { alias: 'tab' });

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);
  private readonly router = inject(Router);
  private readonly activatedRoute = inject(ActivatedRoute);
  private readonly history = inject(NavigationHistory);

  protected readonly tabs = TABS;
  protected readonly tab = signal<TabId>('overview');

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.activatedRoute,
      queryParams: { tab: id === 'overview' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  /** Back where the user came from: a service is opened from the flat list as
   *  well as from its environment's resource table. */
  protected backLink(): UrlTree {
    return this.history.backTo('/services');
  }

  protected readonly service = signal<Service | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  protected readonly deployments = signal<Deployment[]>([]);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected composeContent = '';
  protected name = '';
  protected description = '';
  protected connectToPredefinedNetwork = false;
  protected accessProtection: 'none' | 'basic_auth' | 'sso' = 'none';
  protected accessBasicAuth = '';
  protected noindex = false;

  protected readonly accessLabel = computed(() => {
    switch (this.service()?.access_protection ?? 'none') {
      case 'sso':
        return 'AkerDock login (team members only)';
      case 'basic_auth':
        return 'Basic auth (shared credentials)';
      default:
        return 'Public';
    }
  });

  /**
   * An inline stack builds nothing — its file IS the source (§9.1) — so
   * "Deploy" already reapplies the current file and variables, recreating
   * only the services whose configuration changed (compose-spec §8.2).
   * "Restart" is listed for what it is: the containers as they stand, with
   * the variables they were created with.
   */
  protected readonly actions = computed<ActionItem[]>(() => {
    const items: ActionItem[] = [
      {
        id: 'restart',
        label: 'Restart',
        icon: 'rotate-cw',
        hint: 'Restart the containers as they stand — keeps their current variables',
      },
    ];
    if (this.service()?.desired_status === 'stopped') {
      items.push({ id: 'start', label: 'Start', icon: 'play', hint: 'Start the stack again' });
    } else {
      items.push({
        id: 'stop',
        label: 'Stop',
        icon: 'square',
        danger: true,
        hint: 'Stop every container of the stack — the services go offline',
      });
    }
    return items;
  });

  constructor() {
    // URL → state: seeds the tab on load and follows back/forward. A tab the
    // URL does not name falls back to the first one.
    effect(() => {
      const wanted = this.tabParam();
      this.tab.set(TABS.find((t) => t.id === wanted)?.id ?? TABS[0].id);
    });
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    const client = this.api.client();
    try {
      const [svc, comps, deps] = await Promise.all([
        client.getService(uuid),
        client.listServiceComponents(uuid),
        client.listServiceDeployments(uuid, { limit: 10 }),
      ]);
      this.service.set(svc);
      this.seedForms(svc);
      this.components.set(comps.data);
      this.deployments.set(deps.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  private seedForms(svc: Service): void {
    this.composeContent = svc.compose_content;
    this.name = svc.name;
    this.description = svc.description ?? '';
    this.connectToPredefinedNetwork = svc.connect_to_predefined_network ?? false;
    this.accessProtection = svc.access_protection ?? 'none';
    this.accessBasicAuth = '';
    this.noindex = svc.noindex ?? false;
  }

  protected saveCompose(): Promise<void> {
    return this.patch(
      { compose_content: this.composeContent },
      'Saved. The file is validated now and applied at the next deployment.',
    );
  }

  protected saveSettings(): Promise<void> {
    if (!this.name.trim()) return Promise.resolve();
    return this.patch(
      {
        name: this.name.trim(),
        description: this.description.trim() || null,
        connect_to_predefined_network: this.connectToPredefinedNetwork,
      },
      'Saved. A network change applies at the next deployment.',
    );
  }

  protected saveAccess(): Promise<void> {
    return this.patch(
      {
        access_protection: this.accessProtection,
        // Empty means "keep what is stored" — the API generates credentials
        // itself when basic auth is switched on without any.
        ...(this.accessBasicAuth.trim() ? { access_basic_auth: this.accessBasicAuth.trim() } : {}),
        noindex: this.noindex,
      },
      'Saved. The access wall is updated immediately.',
    );
  }

  /** One PATCH per form: each tab sends its own fields, so saving the access
   * wall cannot resubmit a compose file the operator was still editing. */
  private async patch(body: ServiceUpdate, notice: string): Promise<void> {
    const svc = this.service();
    if (!svc || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateService(this.uuid(), svc.version!, body);
      this.service.set(updated);
      this.seedForms(updated);
      this.notice.set(notice);
    } catch (err) {
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.load(this.uuid());
        this.error.set(
          'Your edit raced a concurrent change: the latest version was reloaded. Re-apply your edit on top of it.',
        );
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }

  protected async run(action: 'deploy' | 'start' | 'stop' | 'restart'): Promise<void> {
    const client = this.api.client();
    this.busy.set(true);
    this.error.set(null);
    try {
      switch (action) {
        case 'deploy':
          await client.deployService(this.uuid());
          break;
        case 'start':
          await client.startService(this.uuid());
          break;
        case 'stop':
          await client.stopService(this.uuid());
          break;
        case 'restart':
          await client.restartService(this.uuid());
          break;
      }
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    const svc = this.service();
    if (!svc || this.busy()) return;
    if (
      !(await this.confirm.ask({
        title: 'Delete the stack',
        message: `Delete the stack "${svc.name}"? Containers and network are removed; volumes are kept.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteService(this.uuid());
      await this.router.navigate(['/services']);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
