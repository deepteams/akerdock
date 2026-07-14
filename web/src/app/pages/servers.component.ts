import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Server = components['schemas']['Server'];
type PrivateKey = components['schemas']['PrivateKey'];

@Component({
  selector: 'app-servers',
  standalone: true,
  imports: [FormsModule, RouterLink, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Servers</h1>
        <button class="akd-btn" type="button" (click)="toggleCreate()">
          {{ creating() ? 'Cancel' : 'New server' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label for="sv-name">Name</label>
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
            <label for="sv-host">Host (IP or FQDN, reachable over SSH)</label>
            <input
              id="sv-host"
              name="host"
              class="akd-input akd-mono"
              required
              [(ngModel)]="host"
              [disabled]="busy()"
            />
          </div>
          <div class="row">
            <div class="akd-field">
              <label for="sv-port">SSH port</label>
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
              <label for="sv-user">SSH user</label>
              <input
                id="sv-user"
                name="user"
                class="akd-input akd-mono"
                [(ngModel)]="user"
                [disabled]="busy()"
              />
            </div>
          </div>
          <div class="akd-field">
            <label for="sv-key">Private key</label>
            <select
              id="sv-key"
              name="privateKey"
              class="akd-select"
              [(ngModel)]="privateKeyUuid"
              [disabled]="busy()"
            >
              <option value="" disabled>Choose a key…</option>
              @for (key of privateKeys(); track key.uuid) {
                <option [value]="key.uuid">{{ key.name }}</option>
              }
            </select>
          </div>
          <label class="check">
            <input
              type="checkbox"
              name="isBuildServer"
              [(ngModel)]="isBuildServer"
              [disabled]="busy()"
            />
            Dedicated build server (cannot host applications)
          </label>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !valid()">
              {{ busy() ? 'Creating…' : 'Create server' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (servers().length === 0) {
        <div class="akd-empty">
          <p><strong>No servers yet.</strong></p>
          <p>Register an SSH-reachable host; validation installs and checks Docker.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Servers of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Host</th>
              <th scope="col">Status</th>
              <th scope="col">Role</th>
              <th scope="col">Docker</th>
            </tr>
          </thead>
          <tbody>
            @for (server of servers(); track server.uuid) {
              <tr>
                <td>
                  <a [routerLink]="['/servers', server.uuid]">{{ server.name }}</a>
                </td>
                <td class="akd-mono">{{ server.user }}&#64;{{ server.host }}:{{ server.port }}</td>
                <td><akd-status-badge domain="resource" [state]="server.status" /></td>
                <td class="akd-muted">{{ server.is_build_server ? 'build' : 'deploy' }}</td>
                <td class="akd-muted">{{ server.docker_version ?? '—' }}</td>
              </tr>
            }
          </tbody>
        </table>
      }
    </div>
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--akd-space-5);
        max-width: 32rem;
      }
      .row {
        display: flex;
        gap: var(--akd-space-2);
      }
      .row .grow {
        flex: 1;
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
    `,
  ],
})
export class ServersComponent {
  private readonly api = inject(ApiService);

  protected readonly servers = signal<Server[]>([]);
  protected readonly privateKeys = signal<PrivateKey[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);

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
