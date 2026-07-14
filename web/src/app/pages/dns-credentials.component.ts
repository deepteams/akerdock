import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type DnsCredential = components['schemas']['DnsCredential'];

@Component({
  selector: 'app-dns-credentials',
  standalone: true,
  imports: [FormsModule, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>DNS credentials</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <h2>Add a credential</h2>
        <p class="akd-muted">
          Used for DNS-01 challenges (wildcard certificates). The provider is a Lego provider id
          — cloudflare, route53, ovh…
        </p>
        <form class="form" (ngSubmit)="create()">
          <div class="row">
            <div class="akd-field">
              <label for="dns-name">Name</label>
              <input
                id="dns-name"
                name="name"
                class="akd-input"
                placeholder="e.g. cloudflare-prod"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label for="dns-provider">Provider</label>
              <input
                id="dns-provider"
                name="provider"
                class="akd-input"
                placeholder="e.g. cloudflare"
                [(ngModel)]="provider"
                [disabled]="busy()"
                required
              />
            </div>
          </div>
          <div class="akd-field">
            <label for="dns-config">Provider variables (KEY=VALUE, one per line)</label>
            <textarea
              id="dns-config"
              name="config"
              class="akd-textarea"
              rows="4"
              placeholder="CLOUDFLARE_DNS_API_TOKEN=…"
              [(ngModel)]="config"
              [disabled]="busy()"
              required
            ></textarea>
            <p class="akd-muted hint">
              The variables expected by the Lego provider. They are write-only: encrypted at rest
              and never returned by the API.
            </p>
          </div>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy()">Add credential</button>
          </div>
        </form>
      </section>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (credentials().length === 0) {
        <div class="akd-empty">
          <p><strong>No DNS credentials yet.</strong></p>
          <p>Wildcard certificates need one to answer DNS-01 challenges.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">DNS-01 credentials of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Provider</th>
              <th scope="col">In use</th>
              <th scope="col">Created</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (cred of credentials(); track cred.uuid) {
              <tr>
                <td>{{ cred.name }}</td>
                <td class="akd-mono">{{ cred.provider }}</td>
                <td class="akd-muted">{{ cred.in_use ? 'yes' : 'no' }}</td>
                <td class="akd-muted">
                  {{ cred.created_at ? (cred.created_at | slice: 0 : 10) : '—' }}
                </td>
                <td class="right">
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(cred)"
                  >
                    Delete
                  </button>
                </td>
              </tr>
            }
          </tbody>
        </table>
      }
    </div>
  `,
  styles: [
    `
      .form {
        display: grid;
        gap: var(--akd-space-3);
      }
      .row {
        display: flex;
        gap: var(--akd-space-3);
        flex-wrap: wrap;
      }
      .row .akd-field {
        flex: 1;
        min-width: 200px;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
    `,
  ],
})
export class DnsCredentialsComponent {
  private readonly api = inject(ApiService);

  protected readonly credentials = signal<DnsCredential[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected name = '';
  protected provider = '';
  protected config = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listDnsCredentials({ limit: 100 });
      this.credentials.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (!this.name.trim() || !this.provider.trim()) return;
    const config: Record<string, string> = {};
    for (const line of this.config.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const eq = trimmed.indexOf('=');
      if (eq <= 0) {
        this.error.set(`Invalid line "${trimmed}" — expected KEY=VALUE.`);
        return;
      }
      config[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1);
    }
    if (Object.keys(config).length === 0) {
      this.error.set('At least one provider variable is required.');
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createDnsCredential({
        name: this.name.trim(),
        provider: this.provider.trim(),
        config,
      });
      this.name = '';
      this.provider = '';
      this.config = '';
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(cred: DnsCredential): Promise<void> {
    if (
      !confirm(
        `Delete the credential "${cred.name}"? Wildcard certificates relying on it will stop renewing.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteDnsCredential(cred.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
