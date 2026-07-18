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
import { ApiService } from '../core/api.service';
import { BreadcrumbComponent, type Crumb } from '../../ui/breadcrumb/breadcrumb.component';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import type { components } from '../../api/schema';

type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];

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
    BreadcrumbComponent,
    CardComponent,
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

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else {
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
      }
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
      const [project, environment, apps, services, databases] = await Promise.all([
        this.api.client().getProject(uuid),
        this.api.client().getEnvironment(uuid, envUuid),
        this.api.client().listApplications({ environment_uuid: envUuid, limit: 100 }),
        this.api.client().listServices({ limit: 100 }),
        this.api.client().listDatabases({ environment_uuid: envUuid, limit: 100 }),
      ]);
      this.project.set(project);
      this.environment.set(environment);
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
}
