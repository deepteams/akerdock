import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { CardComponent } from '../../ui/card/card.component';
import { StatComponent } from '../../ui/stat/stat.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Server = components['schemas']['Server'];
type PrivateKey = components['schemas']['PrivateKey'];

@Component({
  selector: 'app-servers',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    StatusBadgeComponent,
    CardComponent,
    StatComponent,
    IconComponent,
    EmptyStateComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Servers</h1>
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="toggleCreate()">
          <akd-icon [name]="creating() ? 'x' : 'plus'" [size]="15" />
          {{ creating() ? 'Cancel' : 'Add server' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="sv-name">Name</label>
            <input
              id="sv-name"
              name="name"
              class="akd-input"
              required
              [(ngModel)]="name"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="sv-host">Host</label>
            <input
              id="sv-host"
              name="host"
              class="akd-input akd-input--mono"
              required
              [(ngModel)]="host"
              [disabled]="busy()"
            />
            <span class="akd-field__hint">IP or FQDN, reachable over SSH.</span>
          </div>
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="sv-port">SSH port</label>
              <input
                id="sv-port"
                name="port"
                class="akd-input"
                type="number"
                min="1"
                max="65535"
                [(ngModel)]="port"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field grow">
              <label class="akd-field__label" for="sv-user">SSH user</label>
              <input
                id="sv-user"
                name="user"
                class="akd-input akd-input--mono"
                [(ngModel)]="user"
                [disabled]="busy()"
              />
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="sv-key">Private key</label>
            <div class="akd-select">
              <select
                id="sv-key"
                name="privateKey"
                class="akd-input"
                [(ngModel)]="privateKeyUuid"
                [disabled]="busy()"
              >
                <option value="" disabled>Choose a key…</option>
                @for (key of privateKeys(); track key.uuid) {
                  <option [value]="key.uuid">{{ key.name }}</option>
                }
              </select>
            </div>
          </div>
          <label class="akd-check">
            <input
              type="checkbox"
              name="isBuildServer"
              [(ngModel)]="isBuildServer"
              [disabled]="busy()"
            />
            Dedicated build server (cannot host applications)
          </label>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy() || !valid()">
              {{ busy() ? 'Creating…' : 'Create server' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (servers().length === 0) {
        <akd-empty-state
          icon="server"
          title="No servers yet"
          message="Register an SSH-reachable host; validation installs and checks Docker."
        />
      } @else {
        <div class="stack">
          <div class="stats">
            <akd-card>
              <akd-stat label="Servers" [value]="servers().length" [delta]="buildDelta()" />
            </akd-card>
            <akd-card>
              <akd-stat label="Ready" [value]="readyCount()" />
            </akd-card>
            <akd-card>
              <akd-stat label="Unreachable" [value]="unreachableCount()" />
            </akd-card>
          </div>

          <akd-card title="All servers" [padded]="false">
            <table class="akd-table akd-table--clickable">
              <caption class="sr-only">
                Servers of this team
              </caption>
              <thead>
                <tr>
                  <th scope="col">Server</th>
                  <th scope="col">Status</th>
                  <th scope="col">SSH</th>
                  <th scope="col">Proxy</th>
                  <th scope="col">Docker</th>
                </tr>
              </thead>
              <tbody>
                @for (server of servers(); track server.uuid) {
                  <tr (click)="open(server)">
                    <td class="akd-mono">
                      <a [routerLink]="['/servers', server.uuid]">{{ server.name }}</a>
                    </td>
                    <td><akd-status-badge domain="resource" [state]="server.status" /></td>
                    <td class="akd-mono akd-muted">
                      {{ server.user }}&#64;{{ server.host }}:{{ server.port }}
                    </td>
                    <td>
                      @if (server.is_build_server) {
                        <span class="akd-badge akd-badge--mono">build server — no proxy</span>
                      } @else if (server.proxy_type === 'none') {
                        <span class="akd-badge akd-badge--mono">no proxy</span>
                      } @else {
                        <akd-status-badge
                          domain="resource"
                          [state]="server.proxy_observed_status ?? 'unknown'"
                        />
                      }
                    </td>
                    <td class="akd-muted">{{ server.docker_version ?? '—' }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </akd-card>
        </div>
      }
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .stack {
        display: grid;
        gap: var(--space-5);
      }
      .stats {
        display: grid;
        grid-template-columns: repeat(3, 1fr);
        gap: var(--space-4);
      }
      .create {
        display: grid;
        gap: var(--space-4);
        padding: var(--space-5);
        margin-bottom: var(--space-5);
        max-width: 32rem;
      }
      .row {
        display: flex;
        gap: var(--space-3);
      }
      .row .grow {
        flex: 1;
      }
    `,
  ],
})
export class ServersComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly servers = signal<Server[]>([]);
  protected readonly privateKeys = signal<PrivateKey[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);

  protected readonly readyCount = computed(
    () => this.servers().filter((server) => server.status === 'ready').length,
  );
  protected readonly unreachableCount = computed(
    () => this.servers().filter((server) => server.status === 'unreachable').length,
  );
  protected readonly buildDelta = computed(() => {
    const count = this.servers().filter((server) => server.is_build_server).length;
    if (count === 0) return undefined;
    return `incl. ${count} build server${count > 1 ? 's' : ''}`;
  });

  protected name = '';
  protected host = '';
  protected port = 22;
  protected user = 'root';
  protected privateKeyUuid = '';
  protected isBuildServer = false;

  constructor() {
    void this.load();
  }

  protected valid(): boolean {
    return !!(this.name.trim() && this.host.trim() && this.user.trim() && this.privateKeyUuid);
  }

  protected open(server: Server): void {
    void this.router.navigate(['/servers', server.uuid]);
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listServers({ limit: 100 });
      this.servers.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected toggleCreate(): void {
    this.creating.set(!this.creating());
    if (this.creating()) void this.loadKeys();
  }

  private async loadKeys(): Promise<void> {
    try {
      const page = await this.api.client().listPrivateKeys({ limit: 100 });
      this.privateKeys.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createServer({
        name: this.name.trim(),
        host: this.host.trim(),
        port: this.port,
        user: this.user.trim(),
        private_key_uuid: this.privateKeyUuid,
        ssh_timeout_seconds: 30,
        is_build_server: this.isBuildServer,
        proxy_type: 'traefik',
        proxy_http_port: 80,
        proxy_https_port: 443,
      });
      this.name = '';
      this.host = '';
      this.creating.set(false);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
