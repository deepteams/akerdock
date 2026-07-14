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
import { Router, RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
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
  imports: [FormsModule, RouterLink, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div>
          <a routerLink="/services" class="back">← Services</a>
          <h1>{{ service()?.name ?? '…' }}</h1>
        </div>
        @if (service(); as svc) {
          <div class="actions">
            <button class="akd-btn" type="button" [disabled]="busy()" (click)="run('deploy')">
              Deploy
            </button>
            <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="run('restart')">
              Restart
            </button>
            @if (svc.desired_status === 'stopped') {
              <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="run('start')">
                Start
              </button>
            } @else {
              <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="run('stop')">
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
        <section class="cards">
          <div class="akd-card">
            <h2>Desired</h2>
            <akd-status-badge domain="resource" [state]="svc.desired_status" />
          </div>
          <div class="akd-card">
            <h2>Observed</h2>
            <akd-status-badge domain="resource" [state]="svc.observed_status" />
          </div>
        </section>

        @if (components().length > 0) {
          <section class="akd-card block">
            <h2>Components</h2>
            <ul class="component-list">
              @for (c of components(); track c.uuid) {
                <li>
                  <span class="akd-mono">{{ c.name }}</span>
                  @if (c.is_database) {
                    <span class="akd-muted">db: {{ c.database_engine }}</span>
                  }
                  @if (c.exclude_from_hc) {
                    <span class="akd-muted">one-shot</span>
                  }
                  <akd-status-badge domain="resource" [state]="c.observed_status" />
                </li>
              }
            </ul>
          </section>
        }

        <section class="akd-card block">
          <h2>Compose file</h2>
          <form (ngSubmit)="save()">
            <textarea
              name="compose"
              class="akd-textarea akd-mono"
              rows="18"
              aria-label="Compose file content"
              [(ngModel)]="composeContent"
              [disabled]="busy()"
            ></textarea>
            <div class="save-row">
              <button class="akd-btn" type="submit" [disabled]="busy() || !composeContent.trim()">
                {{ busy() ? 'Saving…' : 'Save file' }}
              </button>
              <span class="akd-muted">Validated on save; applied at the next deployment.</span>
            </div>
          </form>
        </section>

        <section class="akd-card block">
          <h2>Deployments</h2>
          @if (deployments().length === 0) {
            <p class="akd-muted">No deployment yet.</p>
          } @else {
            <ul class="deployments">
              @for (d of deployments(); track d.uuid) {
                <li>
                  <akd-status-badge domain="deployment" [state]="d.status" />
                  <span>{{ d.trigger }}</span>
                  <span class="akd-muted">{{ d.created_at }}</span>
                </li>
              }
            </ul>
          }
        </section>

        <section class="akd-card block danger">
          <h2>Danger</h2>
          <p class="akd-muted">
            Deletes the routing, every container of the stack and its network. Volumes are
            kept (INV-008).
          </p>
          <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove()">
            Delete stack
          </button>
        </section>
      }
    </div>
  `,
  styles: [
    `
      .back {
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
      }
      .back:hover {
        text-decoration: underline;
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .cards {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
        gap: var(--akd-space-3);
        margin-bottom: var(--akd-space-5);
      }
      .block {
        margin-bottom: var(--akd-space-5);
        max-width: 60rem;
      }
      .block form {
        display: grid;
        gap: var(--akd-space-3);
      }
      h2 {
        margin: 0 0 var(--akd-space-2);
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.04em;
      }
      .component-list,
      .deployments {
        list-style: none;
        margin: 0;
        padding: 0;
        display: grid;
        gap: var(--akd-space-1);
        font-size: var(--akd-text-sm);
      }
      .component-list li,
      .deployments li {
        display: flex;
        align-items: center;
        gap: var(--akd-space-3);
        padding: var(--akd-space-1) 0;
      }
      .save-row {
        display: flex;
        align-items: center;
        gap: var(--akd-space-3);
      }
    `,
  ],
})
export class ServiceDetailComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

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
    if (!confirm(`Delete the stack "${svc.name}"? Containers and network are removed; volumes are kept.`)) {
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
