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
import { Router, RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { BreadcrumbComponent, type Crumb } from '../../ui/breadcrumb/breadcrumb.component';
import { CardComponent } from '../../ui/card/card.component';
import { DrawerComponent } from '../../ui/drawer/drawer.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];
type SharedVariable = components['schemas']['SharedVariable'];

/** One row of the unified resource table, whatever the underlying kind. */
interface ResourceRow {
  uuid: string;
  kind: 'application' | 'service' | 'database';
  icon: string;
  name: string;
  type: string;
  desired: string;
  observed: string;
  detail: string;
  link: string[];
}

const KIND_ICON: Record<ResourceRow['kind'], string> = {
  application: 'rocket',
  service: 'boxes',
  database: 'database',
};

/**
 * The resources of one environment — the third level of the Projects
 * drill-down (project → environment → resources). Applications and databases
 * are filtered server-side; services are filtered here because /services has
 * no environment filter in the contract yet.
 */
@Component({
  selector: 'app-environment-detail',
  standalone: true,
  imports: [
    RouterLink,
    FormsModule,
    BreadcrumbComponent,
    CardComponent,
    DrawerComponent,
    EmptyStateComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <div class="crumbs">
        <akd-breadcrumb [items]="crumbs()" />
      </div>
      <header class="akd-bar">
        <div class="title">
          <a
            class="akd-iconbtn akd-iconbtn--bordered"
            [routerLink]="['/projects', uuid()]"
            aria-label="Back to environments"
          >
            <akd-icon name="arrow-left" [size]="16" />
          </a>
          <h1 class="title__name">
            <span class="title__project">{{ project()?.name ?? '…' }}</span>
            <span class="title__env">/ {{ environment()?.name ?? '…' }}</span>
          </h1>
        </div>

        <div class="newres">
          <button
            class="akd-btn akd-btn--primary"
            type="button"
            (click)="menu.set(!menu())"
            [attr.aria-expanded]="menu()"
          >
            <akd-icon name="plus" [size]="15" />
            New resource
            <akd-icon name="chevron-down" [size]="13" />
          </button>
          @if (menu()) {
            <div class="newres__menu" role="menu">
              @for (option of newResourceOptions; track option.title) {
                <button
                  class="akd-sidenav__item newres__item"
                  role="menuitem"
                  (click)="create(option)"
                >
                  <span class="newres__icon"><akd-icon [name]="option.icon" [size]="15" /></span>
                  <span class="newres__text">
                    <span class="newres__title">{{ option.title }}</span>
                    <span class="newres__desc">{{ option.desc }}</span>
                  </span>
                </button>
              }
            </div>
          }
        </div>
      </header>

      <nav class="akd-tabs" role="tablist" aria-label="Environment sections">
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="active() === 'resources'"
          [attr.aria-selected]="active() === 'resources'"
          (click)="active.set('resources')"
        >
          Resources
        </button>
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="active() === 'variables'"
          [attr.aria-selected]="active() === 'variables'"
          (click)="active.set('variables')"
        >
          Variables
          @if (variables().length > 0) {
            <span class="akd-tab__count">{{ variables().length }}</span>
          }
        </button>
      </nav>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (active() === 'resources') {
        <akd-card title="Resources" [padded]="false">
          <span card-actions class="akd-badge akd-badge--mono">
            {{ resources().length }} in {{ environment()?.name ?? '…' }}
          </span>
          @if (resources().length === 0) {
            <akd-empty-state
              icon="square-dashed"
              title="No resources yet"
              message="Applications, compose services and databases created in this environment appear here."
            />
          } @else {
            <table class="akd-table akd-table--clickable">
              <caption class="sr-only">
                Resources of this environment
              </caption>
              <thead>
                <tr>
                  <th scope="col">Resource</th>
                  <th scope="col">Type</th>
                  <th scope="col">Desired</th>
                  <th scope="col">Observed</th>
                  <th scope="col">Detail</th>
                </tr>
              </thead>
              <tbody>
                @for (row of resources(); track row.uuid) {
                  <tr (click)="open(row)">
                    <td>
                      <span class="res">
                        <span class="res__icon"><akd-icon [name]="row.icon" [size]="15" /></span>
                        <a
                          class="akd-mono"
                          [routerLink]="row.link"
                          (click)="$event.stopPropagation()"
                        >
                          {{ row.name }}
                        </a>
                      </span>
                    </td>
                    <td>
                      <span class="akd-badge akd-badge--mono">{{ row.type }}</span>
                    </td>
                    <td><akd-status-badge domain="resource" [state]="row.desired" /></td>
                    <td><akd-status-badge domain="resource" [state]="row.observed" /></td>
                    <td class="akd-mono akd-muted">{{ row.detail || '—' }}</td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      } @else {
        <akd-card title="Environment variables" [padded]="false">
          <button
            card-actions
            class="akd-btn akd-btn--primary akd-btn--sm"
            type="button"
            (click)="openAddVar()"
            [disabled]="busy()"
          >
            <akd-icon name="plus" [size]="14" />
            Add variable
          </button>
          @if (variables().length === 0) {
            <akd-empty-state
              icon="hash"
              title="No environment variables"
              message="Shared across every resource of this environment, referenced as {{ '{{' }}environment.KEY{{ '}}' }} in their env."
            />
          } @else {
            <table class="akd-table">
              <caption class="sr-only">
                Environment-scoped shared variables
              </caption>
              <thead>
                <tr>
                  <th scope="col">Key</th>
                  <th scope="col">Value</th>
                  <th scope="col">Reference</th>
                  <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (v of variables(); track v.uuid) {
                  <tr>
                    <td class="akd-mono">
                      {{ v.key }}
                      @if (v.is_secret) {
                        <span class="akd-badge">secret</span>
                      }
                    </td>
                    <td class="akd-mono akd-muted">
                      {{ v.is_redacted ? '••••••••' : v.value }}
                    </td>
                    <td class="akd-mono akd-muted">{{ '{{' }}environment.{{ v.key }}{{ '}}' }}</td>
                    <td class="right">
                      <button
                        class="akd-btn akd-btn--danger akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="removeVar(v)"
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      }

      <akd-drawer [open]="showAddVar()" title="Add environment variable" (closed)="closeAddVar()">
        <form id="env-var-form" class="varform" (ngSubmit)="createVar()">
          @if (varError(); as message) {
            <p class="akd-error" role="alert">{{ message }}</p>
          }
          <div class="akd-field">
            <label class="akd-field__label" for="ev-key">Key</label>
            <input
              id="ev-key"
              name="key"
              class="akd-input akd-input--mono"
              placeholder="e.g. API_URL"
              [(ngModel)]="varKey"
              [disabled]="busy()"
            />
            <span class="akd-field__hint">Letters, digits and underscore; cannot start with a digit.</span>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="ev-value">Value</label>
            <input
              id="ev-value"
              name="value"
              class="akd-input akd-input--mono"
              [(ngModel)]="varValue"
              [disabled]="busy()"
            />
          </div>
          <label class="switch">
            <input type="checkbox" class="akd-switch" name="secret" [(ngModel)]="varSecret" [disabled]="busy()" />
            <span>
              <span class="switch__label">Secret</span>
              <span class="switch__desc">Encrypted at rest and never shown again (INV-003).</span>
            </span>
          </label>
        </form>
        <div drawer-footer>
          <button class="akd-btn akd-btn--ghost" type="button" (click)="closeAddVar()" [disabled]="busy()">
            Cancel
          </button>
          <button
            class="akd-btn akd-btn--primary"
            type="submit"
            form="env-var-form"
            [disabled]="busy() || !varKey.trim()"
          >
            <akd-icon name="plus" [size]="15" />
            Add variable
          </button>
        </div>
      </akd-drawer>
    </div>
  `,
  styles: [
    `
      .crumbs {
        margin-bottom: var(--space-2);
      }
      .title {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        min-width: 0;
      }
      .akd-bar h1.title__name {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .title__project {
        font-family: var(--font-mono);
      }
      .title__env {
        color: var(--text-3);
        /* Angular strips inter-element whitespace; restore the kit's gap. */
        margin-left: var(--space-2);
      }
      .newres {
        position: relative;
      }
      .newres__menu {
        position: absolute;
        top: 100%;
        right: 0;
        margin-top: 6px;
        width: 300px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        padding: 6px;
        z-index: 50;
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      .newres__item {
        align-items: flex-start;
      }
      .newres__icon {
        color: var(--accent);
        margin-top: 2px;
        line-height: 0;
      }
      .newres__text {
        display: grid;
        gap: 1px;
        flex: 1;
      }
      .newres__title {
        color: var(--text-1);
        font-weight: var(--weight-medium);
      }
      .newres__desc {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .res {
        display: inline-flex;
        align-items: center;
        gap: 10px;
      }
      .res__icon {
        color: var(--text-3);
        line-height: 0;
      }
    `,
  ],
})
export class EnvironmentDetailComponent {
  /** Bound from the route by withComponentInputBinding. */
  readonly uuid = input.required<string>();
  readonly envUuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly project = signal<Project | null>(null);
  protected readonly environment = signal<Environment | null>(null);
  protected readonly resources = signal<ResourceRow[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly menu = signal(false);

  protected readonly active = signal<'resources' | 'variables'>('resources');
  protected readonly variables = signal<SharedVariable[]>([]);
  protected readonly busy = signal(false);
  protected readonly showAddVar = signal(false);
  protected readonly varError = signal<string | null>(null);
  protected varKey = '';
  protected varValue = '';
  protected varSecret = false;

  protected readonly crumbs = computed<Crumb[]>(() => [
    { label: 'Projects', link: '/projects' },
    { label: this.project()?.name ?? '…', link: ['/projects', this.uuid()] },
    { label: this.environment()?.name ?? '…' },
  ]);

  protected readonly newResourceOptions = [
    {
      icon: 'rocket',
      title: 'Application',
      desc: 'From a git repo, a Dockerfile or an image',
      target: '/applications/new',
    },
    {
      icon: 'boxes',
      title: 'Compose service',
      desc: 'Multi-container stack from compose.yaml',
      target: '/services',
    },
    {
      icon: 'database',
      title: 'PostgreSQL database',
      desc: 'Managed — backups, drills, TLS',
      target: '/databases',
    },
  ] as const;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      const envUuid = this.envUuid();
      untracked(() => void this.load(uuid, envUuid));
    });
  }

  private async load(uuid: string, envUuid: string): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      const [project, environment, apps, services, databases, variables] = await Promise.all([
        this.api.client().getProject(uuid),
        this.api.client().getEnvironment(uuid, envUuid),
        this.api.client().listApplications({ environment_uuid: envUuid, limit: 100 }),
        this.api.client().listServices({ limit: 100 }),
        this.api.client().listDatabases({ environment_uuid: envUuid, limit: 100 }),
        // The list filters by scope only, so narrow to THIS environment here.
        this.api.client().listSharedVariables({ scope: 'environment', limit: 100 }),
      ]);
      this.project.set(project);
      this.environment.set(environment);
      this.variables.set(
        variables.data
          .filter((v) => v.environment_uuid === envUuid)
          .sort((a, b) => a.key.localeCompare(b.key)),
      );
      const rows: ResourceRow[] = [
        ...apps.data.map((app): ResourceRow => ({
          uuid: app.uuid,
          kind: 'application',
          icon: KIND_ICON.application,
          name: app.name,
          type: app.build_pack ?? app.source_type,
          desired: app.desired_status,
          observed: app.observed_status,
          detail: app.domains?.[0] ?? '',
          link: ['/applications', app.uuid],
        })),
        // The /services list has no environment filter in the contract yet,
        // hence the client-side narrowing.
        ...services.data
          .filter((service) => service.environment_uuid === envUuid)
          .map((service): ResourceRow => ({
            uuid: service.uuid,
            kind: 'service',
            icon: KIND_ICON.service,
            name: service.name,
            type: 'compose',
            desired: service.desired_status,
            observed: service.observed_status,
            detail: '',
            link: ['/services', service.uuid],
          })),
        ...databases.data.map((db): ResourceRow => ({
          uuid: db.uuid,
          kind: 'database',
          icon: KIND_ICON.database,
          name: db.name,
          type: db.image ?? db.engine,
          desired: db.desired_status,
          observed: db.observed_status,
          detail: db.is_public ? `public :${db.public_port}` : 'internal only',
          link: ['/databases', db.uuid],
        })),
      ];
      rows.sort((a, b) => a.name.localeCompare(b.name));
      this.resources.set(rows);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected open(row: ResourceRow): void {
    void this.router.navigate(row.link);
  }

  protected create(option: (typeof this.newResourceOptions)[number]): void {
    this.menu.set(false);
    void this.router.navigate([option.target], {
      queryParams: {
        create: option.target === '/applications/new' ? null : 1,
        project: this.uuid(),
        environment: this.envUuid(),
      },
    });
  }

  protected openAddVar(): void {
    this.varKey = '';
    this.varValue = '';
    this.varSecret = false;
    this.varError.set(null);
    this.showAddVar.set(true);
  }

  protected closeAddVar(): void {
    if (this.busy()) return;
    this.showAddVar.set(false);
  }

  protected async createVar(): Promise<void> {
    if (this.busy() || !this.varKey.trim()) return;
    this.busy.set(true);
    this.varError.set(null);
    try {
      await this.api.client().createSharedVariable({
        scope: 'environment',
        environment_uuid: this.envUuid(),
        key: this.varKey.trim(),
        value: this.varValue,
        is_secret: this.varSecret,
      });
      this.showAddVar.set(false);
      await this.reloadVariables();
    } catch (err) {
      this.varError.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeVar(v: SharedVariable): Promise<void> {
    if (!confirm(`Delete the environment variable "${v.key}"? Resources pick it up at their next deployment.`)) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteSharedVariable(v.uuid);
      await this.reloadVariables();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async reloadVariables(): Promise<void> {
    const page = await this.api.client().listSharedVariables({ scope: 'environment', limit: 100 });
    this.variables.set(
      page.data
        .filter((v) => v.environment_uuid === this.envUuid())
        .sort((a, b) => a.key.localeCompare(b.key)),
    );
  }
}
