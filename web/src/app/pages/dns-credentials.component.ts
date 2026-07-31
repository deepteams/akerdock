import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type DnsCredential = components['schemas']['DnsCredential'];

@Component({
  selector: 'app-dns-credentials',
  standalone: true,
  imports: [FormsModule, SlicePipe, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>DNS credentials</h2>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <akd-card title="Add a credential" class="create">
        <form class="fields" (ngSubmit)="create()">
          <p class="intro">
            Used for DNS-01 challenges (wildcard certificates). The provider is a Lego provider id —
            cloudflare, route53, ovh…
          </p>
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="dns-name">Name</label>
              <input
                id="dns-name"
                name="name"
                class="akd-input akd-input--mono"
                placeholder="e.g. cloudflare-prod"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="dns-provider">Provider</label>
              <input
                id="dns-provider"
                name="provider"
                class="akd-input akd-input--mono"
                placeholder="e.g. cloudflare"
                [(ngModel)]="provider"
                [disabled]="busy()"
                required
              />
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="dns-config">
              Provider variables (KEY=VALUE, one per line)
            </label>
            <textarea
              id="dns-config"
              name="config"
              class="akd-input akd-input--mono"
              rows="4"
              placeholder="CLOUDFLARE_DNS_API_TOKEN=…"
              [(ngModel)]="config"
              [disabled]="busy()"
              required
            ></textarea>
            <span class="akd-field__hint">
              The variables expected by the Lego provider. They are write-only: encrypted at rest
              and never returned by the API.
            </span>
          </div>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              <akd-icon name="plus" [size]="15" />
              Add credential
            </button>
          </div>
        </form>
      </akd-card>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (credentials().length === 0) {
        <akd-empty-state
          icon="globe"
          title="No DNS credentials yet"
          message="Wildcard certificates need one to answer DNS-01 challenges."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              DNS-01 credentials of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Provider</th>
                <th scope="col">In use</th>
                <th scope="col">Created</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (cred of credentials(); track cred.uuid) {
                <tr>
                  <td class="akd-mono">{{ cred.name }}</td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ cred.provider }}</span>
                  </td>
                  <td>
                    @if (cred.in_use) {
                      <span class="akd-badge akd-badge--accent">in use</span>
                    } @else {
                      <span class="akd-badge">unused</span>
                    }
                  </td>
                  <td class="akd-muted">
                    {{ cred.created_at ? (cred.created_at | slice: 0 : 10) : '—' }}
                  </td>
                  <td class="right">
                    <button
                      class="akd-iconbtn"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(cred)"
                      aria-label="Delete credential"
                    >
                      <akd-icon name="trash-2" [size]="15" />
                    </button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--space-5);
        max-width: 640px;
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .intro {
        margin: 0;
        font-size: var(--text-sm);
        color: var(--text-2);
      }
      .row {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
        gap: var(--space-4);
      }
    `,
  ],
})
export class DnsCredentialsComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

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
      const credentials = await fetchAll((cursor) =>
        this.api.client().listDnsCredentials({ limit: 100, cursor }),
      );
      this.credentials.set(credentials);
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
      !(await this.confirm.ask({
        title: 'Delete the credential',
        message: `Delete the credential "${cred.name}"? Wildcard certificates relying on it will stop renewing.`,
        confirmLabel: 'Delete',
      }))
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
