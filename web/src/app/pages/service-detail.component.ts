import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink, UrlTree } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import { NavigationHistory } from '../core/navigation-history.service';
import { ApiError } from '../../api/client';
import type { components } from '../../api/schema';

type Service = components['schemas']['Service'];
type ServiceComponent = components['schemas']['ServiceComponent'];
type Deployment = components['schemas']['Deployment'];

/**
 * One compose stack: the file is the source of truth, edited here and
 * validated by the API at every save (compose-spec §11).
 */
@Component({
  selector: 'app-service-detail',
  standalone: true,
  imports: [FormsModule, RouterLink, CardComponent, IconComponent, StatusBadgeComponent],
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
        @if (service(); as svc) {
          <div class="actions">
            <button
              class="akd-btn akd-btn--primary"
              type="button"
              [disabled]="busy()"
              (click)="run('deploy')"
            >
              <akd-icon name="rocket" [size]="15" />
              Deploy
            </button>
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              [disabled]="busy()"
              (click)="run('restart')"
            >
              <akd-icon name="rotate-cw" [size]="15" />
              Restart
            </button>
            @if (svc.desired_status === 'stopped') {
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
                <akd-icon name="square" [size]="15" />
                Stop
              </button>
            }
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
        <div class="stack">
          <section class="cards">
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

          <akd-card title="Compose file">
            <form class="compose-form" (ngSubmit)="save()">
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
                <span class="akd-muted">Validated on save; applied at the next deployment.</span>
              </div>
            </form>
          </akd-card>

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

          <div class="akd-card danger">
            <div class="akd-card__header">
              <h2 class="akd-card__title">Danger</h2>
            </div>
            <div class="akd-card__body danger-body">
              <p class="akd-muted">
                Deletes the routing, every container of the stack and its network. Volumes are kept
                (INV-008).
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

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);
  private readonly history = inject(NavigationHistory);

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

  constructor() {
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
      this.composeContent = svc.compose_content;
      this.components.set(comps.data);
      this.deployments.set(deps.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async save(): Promise<void> {
    const svc = this.service();
    if (!svc || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateService(this.uuid(), svc.version!, {
        compose_content: this.composeContent,
      });
      this.service.set(updated);
      this.composeContent = updated.compose_content;
      this.notice.set('File saved — applied at the next deployment.');
    } catch (err) {
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.load(this.uuid());
        this.error.set(
          'Your edit raced a concurrent change: the latest file was reloaded. Re-apply your edit on top of it.',
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
      !confirm(
        `Delete the stack "${svc.name}"? Containers and network are removed; volumes are kept.`,
      )
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
