import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { DrawerComponent } from '../../../ui/drawer/drawer.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { fetchAll } from '../../core/pagination';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import type { components } from '../../../api/schema';

type IngressEndpoint = components['schemas']['IngressEndpoint'];
type Server = components['schemas']['Server'];
type IngressAccess = 'sso' | 'basic_auth' | 'none';

/**
 * The declared ingress endpoints (ADR-060) — the mirror of the bastion. Each is
 * a stable public URL relayed to whoever runs `akerdock ingress <name> <port>`
 * on their machine. Declaring one publishes a hostname onto laptop software, so
 * it is an admin act; the wall (SSO by default) protects the visitor side.
 */
@Component({
  selector: 'app-ingress-endpoints-tab',
  standalone: true,
  imports: [FormsModule, CardComponent, DrawerComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="bar">
      <button class="akd-btn akd-btn--primary" type="button" (click)="openNew()">
        <akd-icon name="plus" [size]="15" />
        Declare ingress
      </button>
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (endpoints().length === 0) {
      <akd-empty-state
        icon="globe"
        title="No ingress endpoint yet"
        message="Declare one to give a stable public URL to a service running on a developer's machine — for a webhook, an OAuth callback, or a live demo."
      />
    } @else {
      <akd-card title="Declared ingress endpoints">
        <table class="akd-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Public URL</th>
              <th>Access</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            @for (endpoint of endpoints(); track endpoint.uuid) {
              <tr>
                <td>
                  <strong>{{ endpoint.name }}</strong>
                  @if (endpoint.description) {
                    <span class="akd-muted"> — {{ endpoint.description }}</span>
                  }
                </td>
                <td>
                  <a class="akd-mono" [href]="endpoint.url" target="_blank" rel="noopener">
                    {{ endpoint.fqdn }}
                  </a>
                </td>
                <td>
                  <span class="akd-badge" [class.akd-badge--warn]="endpoint.access === 'none'">
                    {{ accessLabel(endpoint.access) }}
                  </span>
                </td>
                <td>
                  @if (endpoint.occupied) {
                    <span class="akd-badge akd-badge--ok">
                      live{{ endpoint.occupant_email ? ' · ' + endpoint.occupant_email : '' }}
                    </span>
                  } @else {
                    <span class="akd-muted">offline</span>
                  }
                </td>
                <td class="actions">
                  <button
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    type="button"
                    (click)="openEdit(endpoint)"
                    [disabled]="busy()"
                  >
                    Edit
                  </button>
                  <button
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    type="button"
                    (click)="remove(endpoint)"
                    [disabled]="busy()"
                  >
                    Delete
                  </button>
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }

    <akd-drawer
      [open]="showForm()"
      [title]="editing() ? 'Edit ingress endpoint' : 'Declare an ingress endpoint'"
      (closed)="closeForm()"
    >
      <form id="ingress-form" class="fields" (ngSubmit)="submit()">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <p class="intro">
          A stable public URL that relays to a port on a developer's machine. The URL never
          changes, so it can be registered as a webhook target or bookmarked. Visitors reach it over
          HTTPS; it stays out of search engines.
        </p>
        <div class="akd-field">
          <label class="akd-field__label" for="in-name">Name</label>
          <input
            id="in-name"
            name="name"
            class="akd-input akd-input--mono"
            placeholder="e.g. dev-kedric"
            [(ngModel)]="name"
            [disabled]="busy()"
            required
          />
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="in-description">Description</label>
          <input
            id="in-description"
            name="description"
            class="akd-input"
            placeholder="e.g. Kedric's laptop, Stripe webhook testing"
            [(ngModel)]="description"
            [disabled]="busy()"
          />
        </div>
        @if (!editing()) {
          <div class="akd-field">
            <label class="akd-field__label" for="in-server">Ingress server</label>
            <div class="akd-select">
              <select
                id="in-server"
                name="server"
                class="akd-input"
                [ngModel]="serverUuid"
                (ngModelChange)="onServerChange($event)"
                [disabled]="busy()"
                required
              >
                <option value="">Select a server…</option>
                @for (server of servers(); track server.uuid) {
                  <option [value]="server.uuid">{{ server.name }}</option>
                }
              </select>
            </div>
            <span class="akd-field__hint">
              The server whose proxy terminates the hostname and relays to the laptop.
            </span>
          </div>

          @if (serverUuid) {
            <div class="akd-field">
              <label class="akd-field__label" for="in-subdomain">Public hostname</label>
              @if (wildcardSuffix === CUSTOM) {
                <input
                  id="in-subdomain"
                  name="fqdn"
                  class="akd-input akd-input--mono"
                  placeholder="e.g. hooks.example.com"
                  [(ngModel)]="customFqdn"
                  [disabled]="busy()"
                  required
                />
              } @else {
                <div class="subdomain">
                  <input
                    id="in-subdomain"
                    name="subdomain"
                    class="akd-input akd-input--mono"
                    placeholder="dev-kedric"
                    [(ngModel)]="subdomain"
                    [disabled]="busy()"
                    required
                  />
                  <div class="akd-select suffix">
                    <select
                      name="wildcard"
                      class="akd-input akd-input--mono"
                      [(ngModel)]="wildcardSuffix"
                      [disabled]="busy()"
                    >
                      @for (w of wildcards(); track w) {
                        <option [value]="w">.{{ w }}</option>
                      }
                      <option [value]="CUSTOM">Other domain…</option>
                    </select>
                  </div>
                </div>
              }
              <span class="akd-field__hint">
                @if (wildcardSuffix === CUSTOM) {
                  A full hostname you route to this server yourself. Immutable after declaration.
                } @else if (subdomain.trim()) {
                  Your URL will be <strong class="akd-mono">{{ composedFqdn() }}</strong>. Immutable
                  after declaration.
                } @else {
                  A subdomain under the server's wildcard. Immutable after declaration.
                }
              </span>
            </div>
          }
        }
        <div class="akd-field">
          <label class="akd-field__label" for="in-access">Access</label>
          <div class="akd-select">
            <select
              id="in-access"
              name="access"
              class="akd-input"
              [(ngModel)]="access"
              [disabled]="busy()"
            >
              <option value="sso">SSO — only your team's signed-in members</option>
              <option value="basic_auth">Basic auth — a shared password</option>
              <option value="none">None — anyone with the URL (public webhook)</option>
            </select>
          </div>
          <span class="akd-field__hint">
            SSO is the default. Choosing <em>None</em> publishes an unauthenticated URL onto the
            developer's machine — the webhook case, and a deliberate one.
          </span>
        </div>
        @if (access === 'basic_auth') {
          <div class="akd-field">
            <label class="akd-field__label" for="in-password">
              Password{{ editing() ? ' (leave blank to keep)' : '' }}
            </label>
            <input
              id="in-password"
              name="password"
              type="password"
              class="akd-input akd-input--mono"
              [(ngModel)]="basicAuthPassword"
              [disabled]="busy()"
            />
            <span class="akd-field__hint">Visitors sign in as <code>akerdock</code>.</span>
          </div>
        }
      </form>
      <div drawer-footer>
        <button class="akd-btn akd-btn--ghost" type="button" (click)="closeForm()" [disabled]="busy()">
          Cancel
        </button>
        <button
          class="akd-btn akd-btn--primary"
          type="submit"
          form="ingress-form"
          [disabled]="busy() || !valid()"
        >
          <akd-icon name="plus" [size]="15" />
          {{ editing() ? 'Save' : 'Declare ingress' }}
        </button>
      </div>
    </akd-drawer>
  `,
  styles: [
    `
      .bar {
        display: flex;
        justify-content: flex-end;
        margin-bottom: var(--space-4);
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .intro {
        margin: 0;
        color: var(--text-muted);
      }
      .actions {
        text-align: right;
        white-space: nowrap;
      }
      .subdomain {
        display: flex;
        align-items: stretch;
        gap: var(--space-2);
      }
      .subdomain input {
        flex: 1 1 auto;
        min-width: 0;
      }
      .subdomain .suffix {
        flex: 0 0 auto;
      }
    `,
  ],
})
export class IngressEndpointsTabComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly endpoints = signal<IngressEndpoint[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly showForm = signal(false);
  protected readonly editing = signal<IngressEndpoint | null>(null);
  protected readonly error = signal<string | null>(null);

  /** Sentinel suffix value that switches the hostname field to a free-text
   * full FQDN — for a domain routed to the server outside its wildcard. */
  protected readonly CUSTOM = '__custom__';

  protected name = '';
  protected description = '';
  protected serverUuid = '';
  protected subdomain = '';
  protected wildcardSuffix = '';
  protected customFqdn = '';
  protected access: IngressAccess = 'sso';
  protected basicAuthPassword = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const [endpoints, servers] = await Promise.all([
        this.api.client().listIngressEndpoints(),
        fetchAll((cursor) => this.api.client().listServers({ limit: 100, cursor })),
      ]);
      this.endpoints.set(endpoints.data);
      this.servers.set(servers);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected accessLabel(access: IngressEndpoint['access']): string {
    switch (access) {
      case 'sso':
        return 'SSO';
      case 'basic_auth':
        return 'basic auth';
      default:
        return 'public';
    }
  }

  /** The wildcard suffixes offered for the chosen server (one per server
   * today, but a list keeps the dropdown honest if that ever changes). */
  protected wildcards(): string[] {
    const server = this.servers().find((s) => s.uuid === this.serverUuid);
    const w = server?.wildcard_domain?.trim();
    return w ? [w] : [];
  }

  /** The FQDN the form will submit: subdomain + wildcard, or the free-text
   * full hostname when the "Other domain…" option is chosen. */
  protected composedFqdn(): string {
    if (this.wildcardSuffix === this.CUSTOM) return this.customFqdn.trim();
    const sub = this.subdomain.trim().replace(/\.+$/, '');
    if (!sub || !this.wildcardSuffix) return '';
    return `${sub}.${this.wildcardSuffix}`;
  }

  /** Picking a server seeds the suffix with its wildcard, or drops straight to
   * the free-text hostname when the server has none. */
  protected onServerChange(uuid: string): void {
    this.serverUuid = uuid;
    const wildcards = this.wildcards();
    this.wildcardSuffix = wildcards.length ? wildcards[0] : this.CUSTOM;
    this.subdomain = '';
    this.customFqdn = '';
  }

  protected openNew(): void {
    this.editing.set(null);
    this.resetForm();
    this.error.set(null);
    this.showForm.set(true);
  }

  protected openEdit(endpoint: IngressEndpoint): void {
    this.editing.set(endpoint);
    this.name = endpoint.name;
    this.description = endpoint.description ?? '';
    this.access = endpoint.access;
    this.basicAuthPassword = '';
    this.error.set(null);
    this.showForm.set(true);
  }

  protected closeForm(): void {
    this.showForm.set(false);
    this.resetForm();
  }

  private resetForm(): void {
    this.name = '';
    this.description = '';
    this.serverUuid = '';
    this.subdomain = '';
    this.wildcardSuffix = '';
    this.customFqdn = '';
    this.access = 'sso';
    this.basicAuthPassword = '';
  }

  protected valid(): boolean {
    if (!this.name.trim()) return false;
    if (!this.editing() && (!this.composedFqdn() || !this.serverUuid)) return false;
    // basic_auth needs a password on creation; on edit, blank keeps the old one.
    if (this.access === 'basic_auth' && !this.editing() && !this.basicAuthPassword) return false;
    return true;
  }

  protected async submit(): Promise<void> {
    if (!this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const endpoint = this.editing();
      if (endpoint) {
        await this.api.client().updateIngressEndpoint(endpoint.uuid, {
          name: this.name.trim(),
          description: this.description.trim() || undefined,
          access: this.access,
          basic_auth_password: this.basicAuthPassword || undefined,
        });
      } else {
        await this.api.client().createIngressEndpoint({
          name: this.name.trim(),
          description: this.description.trim() || undefined,
          fqdn: this.composedFqdn(),
          server_uuid: this.serverUuid,
          access: this.access,
          basic_auth_password: this.basicAuthPassword || undefined,
        });
      }
      this.showForm.set(false);
      this.resetForm();
      await this.load();
    } catch (err) {
      // The drawer stays open on failure — a hostname collision is fixed in
      // place, not by retyping the whole form.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(endpoint: IngressEndpoint): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the ingress endpoint',
        message: `Delete "${endpoint.name}"? A live tunnel to it is cut immediately, and the URL stops resolving.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteIngressEndpoint(endpoint.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
