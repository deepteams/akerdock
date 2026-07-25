import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import type { components } from '../../api/schema';

type TransactionalEmail = components['schemas']['TransactionalEmail'];
type TransactionalEmailSet = components['schemas']['TransactionalEmailSet'];
type EncryptionStatus = components['schemas']['EncryptionStatus'];
type OauthProviderConfig = components['schemas']['OauthProviderConfig'];

/** The six providers of §10.2, in display order. oidc/azure need an issuer. */
const OAUTH_PROVIDERS: { key: string; label: string; needsIssuer: boolean }[] = [
  { key: 'github', label: 'GitHub', needsIssuer: false },
  { key: 'gitlab', label: 'GitLab', needsIssuer: false },
  { key: 'google', label: 'Google', needsIssuer: false },
  { key: 'azure', label: 'Microsoft (Azure AD)', needsIssuer: true },
  { key: 'bitbucket', label: 'Bitbucket', needsIssuer: false },
  { key: 'oidc', label: 'Generic OIDC (Okta, Keycloak…)', needsIssuer: true },
];

@Component({
  selector: 'app-system',
  standalone: true,
  imports: [FormsModule, RouterLink, CardComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Global settings</h1>
        <span class="akd-badge akd-badge--accent akd-badge--mono">instance</span>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <div class="cols">
        <div class="col">
          <akd-card title="Instance">
            <form class="stack" (ngSubmit)="saveInstance()">
              <div class="akd-field">
                <label class="akd-field__label" for="inst-fqdn">FQDN</label>
                <input
                  id="inst-fqdn"
                  name="fqdn"
                  class="akd-input akd-input--mono"
                  placeholder="deploy.example.com"
                  [(ngModel)]="instanceFqdn"
                  [disabled]="busy()"
                />
                <span class="akd-field__hint">
                  A non-empty FQDN means an HTTPS instance: session cookies become Secure (at the
                  next restart of the binary) and invitation / OAuth URLs are built with https://.
                  Leave empty to allow plain HTTP — trusted networks only.
                </span>
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="inst-acme">ACME contact email</label>
                <input
                  id="inst-acme"
                  name="acmeEmail"
                  class="akd-input akd-input--mono"
                  placeholder="ops@example.com"
                  [(ngModel)]="instanceAcmeEmail"
                  [disabled]="busy()"
                />
                <span class="akd-field__hint">
                  Required before any certificate is issued — Let's Encrypt refuses without a valid
                  contact.
                </span>
              </div>
              @if (instanceNotice(); as message) {
                <p class="akd-muted sm" role="status">{{ message }}</p>
              }
              <div>
                <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                  Save instance settings
                </button>
              </div>
            </form>
          </akd-card>

          <akd-card title="Transactional email">
            <div class="stack">
              @if (email(); as em) {
                <p class="akd-muted sm">
                  @if (em.configured) {
                    Configured — {{ em.kind }} sending as {{ em.from }}. While configured,
                    invitation emails are sent automatically instead of handing out a link.
                  } @else {
                    Not configured. Invitations fall back to a one-time link you pass along
                    yourself.
                  }
                </p>
              }
              <form class="stack" (ngSubmit)="saveEmail()">
                <div class="akd-field mode">
                  <label class="akd-field__label" for="em-kind">Mode</label>
                  <div class="akd-select">
                    <select id="em-kind" name="kind" class="akd-input" [(ngModel)]="emailKind">
                      <option value="smtp">smtp</option>
                      <option value="resend">resend</option>
                    </select>
                  </div>
                </div>
                @if (emailKind === 'smtp') {
                  <div class="row">
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-host">Host</label>
                      <input
                        id="em-host"
                        name="host"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="smtpHost"
                        required
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-port">Port</label>
                      <input
                        id="em-port"
                        name="port"
                        type="number"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="smtpPort"
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-enc">Encryption</label>
                      <div class="akd-select">
                        <select
                          id="em-enc"
                          name="encryption"
                          class="akd-input"
                          [(ngModel)]="smtpEncryption"
                        >
                          <option value="starttls">starttls</option>
                          <option value="tls">tls</option>
                          <option value="none">none (local relay only)</option>
                        </select>
                      </div>
                    </div>
                  </div>
                  <div class="row">
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-user">Username (optional)</label>
                      <input
                        id="em-user"
                        name="username"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="smtpUsername"
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-pass">Password (optional)</label>
                      <input
                        id="em-pass"
                        name="password"
                        type="password"
                        class="akd-input akd-input--mono"
                        autocomplete="new-password"
                        [(ngModel)]="smtpPassword"
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-from">From</label>
                      <input
                        id="em-from"
                        name="from"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="emailFrom"
                        required
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-to">
                        Test recipients (comma-separated)
                      </label>
                      <input
                        id="em-to"
                        name="to"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="emailTo"
                        required
                      />
                    </div>
                  </div>
                } @else {
                  <div class="row">
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-key">API key</label>
                      <input
                        id="em-key"
                        name="api_key"
                        type="password"
                        class="akd-input akd-input--mono"
                        autocomplete="new-password"
                        [(ngModel)]="resendApiKey"
                        required
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-from">From</label>
                      <input
                        id="em-from"
                        name="from"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="emailFrom"
                        required
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="em-to">
                        Test recipients (comma-separated)
                      </label>
                      <input
                        id="em-to"
                        name="to"
                        class="akd-input akd-input--mono"
                        [(ngModel)]="emailTo"
                        required
                      />
                    </div>
                  </div>
                }
                <p class="akd-muted xs">
                  Secrets (password, API key) are write-only: encrypted at rest, never returned by
                  the API. Saving sends a verification message to the test recipients — an
                  unreachable relay is refused on the spot.
                </p>
                <div>
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    Save configuration
                  </button>
                </div>
              </form>
            </div>
          </akd-card>

          <akd-card title="API access">
            <div class="stack">
              <label class="switch-row">
                <input
                  type="checkbox"
                  class="akd-switch"
                  name="apiEnabled"
                  [(ngModel)]="apiOn"
                  [disabled]="busy()"
                  (ngModelChange)="toggleApi($event)"
                />
                Public API enabled
              </label>
              <p class="akd-muted sm">
                When off, token, script and CI calls are refused immediately. The dashboard
                keeps working (its session is exempt), so you can turn it back on here.
              </p>
            </div>
          </akd-card>
        </div>

        <div class="col">
          <akd-card title="Sign-in providers (OAuth/OIDC)" [padded]="false">
            <p class="akd-muted sm pad">
              Let the dashboard sign in through an identity provider. The client secret is
              write-only: encrypted at rest, never returned. The provider's redirect URL is
              <span class="akd-mono">{{ callbackHint() }}</span
              >.
            </p>

            <table class="akd-table">
              <caption class="sr-only">
                Configured sign-in providers
              </caption>
              <thead>
                <tr>
                  <th scope="col">Provider</th>
                  <th scope="col">Status</th>
                  <th scope="col">Client ID</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (p of oauthCatalog; track p.key) {
                  <tr>
                    <td>{{ p.label }}</td>
                    <td class="akd-muted">
                      @if (oauthConfigOf(p.key); as cfg) {
                        {{ cfg.enabled ? 'enabled' : 'configured, disabled' }}
                      } @else {
                        not configured
                      }
                    </td>
                    <td class="akd-mono akd-muted">{{ oauthConfigOf(p.key)?.client_id ?? '—' }}</td>
                    <td class="right">
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="editOauth(p.key)"
                      >
                        {{ oauthConfigOf(p.key) ? 'Edit' : 'Configure' }}
                      </button>
                      @if (oauthConfigOf(p.key)) {
                        <button
                          class="akd-btn akd-btn--danger akd-btn--sm"
                          type="button"
                          [disabled]="busy()"
                          (click)="removeOauth(p.key)"
                        >
                          Remove
                        </button>
                      }
                    </td>
                  </tr>
                }
              </tbody>
            </table>

            @if (oauthEditing(); as key) {
              <form class="stack pad" (ngSubmit)="saveOauth()">
                <h3>{{ oauthLabel(key) }}</h3>
                <div class="row">
                  <div class="akd-field">
                    <label class="akd-field__label" for="oa-client-id">Client ID</label>
                    <input
                      id="oa-client-id"
                      name="client_id"
                      class="akd-input akd-input--mono"
                      [(ngModel)]="oauthClientId"
                      required
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="oa-client-secret">Client secret</label>
                    <input
                      id="oa-client-secret"
                      name="client_secret"
                      type="password"
                      class="akd-input akd-input--mono"
                      autocomplete="new-password"
                      [(ngModel)]="oauthClientSecret"
                      required
                    />
                  </div>
                </div>
                @if (oauthNeedsIssuer(key)) {
                  <div class="row">
                    <div class="akd-field">
                      <label class="akd-field__label" for="oa-issuer">
                        OpenID Connect issuer URL
                      </label>
                      <input
                        id="oa-issuer"
                        name="issuer_url"
                        class="akd-input akd-input--mono"
                        placeholder="https://your-idp.example.com"
                        [(ngModel)]="oauthIssuer"
                        required
                      />
                    </div>
                    <div class="akd-field">
                      <label class="akd-field__label" for="oa-name">Button label (optional)</label>
                      <input
                        id="oa-name"
                        name="display_name"
                        class="akd-input"
                        placeholder="e.g. Okta"
                        [(ngModel)]="oauthDisplayName"
                      />
                    </div>
                  </div>
                }
                <label class="switch-row">
                  <input
                    type="checkbox"
                    class="akd-switch"
                    name="enabled"
                    [(ngModel)]="oauthEnabled"
                  />
                  Show on the sign-in page
                </label>
                <div class="actions">
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    Save provider
                  </button>
                  <button
                    class="akd-btn akd-btn--ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="oauthEditing.set(null)"
                  >
                    Cancel
                  </button>
                </div>
              </form>
            }
          </akd-card>

          <akd-card title="Encryption" [padded]="false">
            @if (encryption(); as enc) {
              <div class="pad">
                <dl class="akd-dl">
                  <dt>Active key version</dt>
                  <dd>{{ enc.active_key_version }}</dd>
                  @if (enc.rotation_job_uuid; as jobUuid) {
                    <dt>Rotation in progress</dt>
                    <dd>
                      <a [routerLink]="['/jobs', jobUuid]" class="akd-mono">{{ jobUuid }}</a>
                    </dd>
                  }
                </dl>
              </div>
              @if (enc.key_versions.length > 0) {
                <table class="akd-table">
                  <caption class="sr-only">
                    Encrypted rows per key version
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Key version</th>
                      <th scope="col">Rows</th>
                      <th scope="col">Columns</th>
                    </tr>
                  </thead>
                  <tbody>
                    @for (kv of enc.key_versions; track kv.key_version) {
                      <tr>
                        <td>
                          {{ kv.key_version }}
                          @if (kv.key_version === enc.active_key_version) {
                            <span class="akd-muted">(active)</span>
                          }
                        </td>
                        <td>{{ kv.total_rows }}</td>
                        <td class="akd-mono">
                          @for (col of kv.columns ?? []; track col.table + col.column) {
                            <span class="cell-line">
                              {{ col.table }}.{{ col.column }} ({{ col.rows }})
                            </span>
                          } @empty {
                            —
                          }
                        </td>
                      </tr>
                    }
                  </tbody>
                </table>
              }
            } @else {
              <p class="akd-muted sm pad">Loading…</p>
            }
            <div class="stack pad">
              @if (encryption()?.key_versions?.length) {
                <p class="akd-muted xs">
                  A rotation has converged when only the active version is still referenced.
                </p>
              }
              <div>
                <button
                  class="akd-btn akd-btn--ghost akd-btn--sm"
                  type="button"
                  [disabled]="busy()"
                  (click)="rotate()"
                >
                  Rotate encryption key
                </button>
              </div>
            </div>
          </akd-card>
        </div>
      </div>
    </div>
  `,
  styles: [
    `
      .cols {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: var(--space-4);
        align-items: start;
      }
      @media (max-width: 1100px) {
        .cols {
          grid-template-columns: 1fr;
        }
      }
      .col {
        display: grid;
        gap: var(--space-4);
        min-width: 0;
      }
      .stack {
        display: grid;
        gap: var(--space-3);
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .row {
        display: flex;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .row .akd-field {
        flex: 1;
        min-width: 180px;
      }
      .mode {
        max-width: 200px;
      }
      .sm {
        font-size: var(--text-sm);
        margin: 0;
      }
      .xs {
        font-size: var(--text-xs);
        margin: 0;
      }
      .actions {
        display: flex;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .switch-row {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        font-size: var(--text-sm);
        color: var(--text-1);
        cursor: pointer;
      }
      .right {
        text-align: right;
        white-space: nowrap;
      }
      .cell-line {
        display: block;
      }
      h3 {
        margin: 0;
        font: var(--weight-semibold) var(--text-md) var(--font-display);
        color: var(--text-1);
      }
    `,
  ],
})
export class SystemComponent {
  private readonly api = inject(ApiService);

  protected readonly email = signal<TransactionalEmail | null>(null);
  protected readonly encryption = signal<EncryptionStatus | null>(null);
  protected apiOn = true;
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected readonly oauthCatalog = OAUTH_PROVIDERS;
  protected readonly oauthConfigs = signal<OauthProviderConfig[]>([]);
  protected readonly oauthEditing = signal<string | null>(null);
  protected oauthClientId = '';
  protected oauthClientSecret = '';
  protected oauthIssuer = '';
  protected oauthDisplayName = '';
  protected oauthEnabled = true;

  protected readonly instanceNotice = signal<string | null>(null);
  protected instanceFqdn = '';
  protected instanceAcmeEmail = '';

  protected emailKind: 'smtp' | 'resend' = 'smtp';
  protected emailFrom = '';
  protected emailTo = '';
  protected smtpHost = '';
  protected smtpPort = 587;
  protected smtpUsername = '';
  protected smtpPassword = '';
  protected smtpEncryption: 'starttls' | 'tls' | 'none' = 'starttls';
  protected resendApiKey = '';

  constructor() {
    void this.load();
  }

  protected async saveInstance(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.instanceNotice.set(null);
    try {
      const updated = await this.api.client().setInstanceSettings({
        fqdn: this.instanceFqdn.trim() || null,
        acme_email: this.instanceAcmeEmail.trim() || null,
      });
      this.instanceFqdn = updated.fqdn ?? '';
      this.instanceAcmeEmail = updated.acme_email ?? '';
      this.instanceNotice.set(
        updated.fqdn
          ? 'Saved. Session cookie security follows the FQDN at the next restart of the binary.'
          : 'Saved — no FQDN: plain-HTTP sign-in is allowed after the next restart of the binary.',
      );
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async load(): Promise<void> {
    const client = this.api.client();
    try {
      const [instance, email, encryption, oauth] = await Promise.all([
        client.getInstanceSettings(),
        client.getTransactionalEmail(),
        client.getEncryptionStatus(),
        client.listOauthProviders(),
      ]);
      this.instanceFqdn = instance.fqdn ?? '';
      this.instanceAcmeEmail = instance.acme_email ?? '';
      this.apiOn = instance.api_enabled ?? true;
      this.email.set(email);
      this.encryption.set(encryption);
      this.oauthConfigs.set(oauth.data);
      if (email.kind) this.emailKind = email.kind;
      if (email.from) this.emailFrom = email.from;
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async saveEmail(): Promise<void> {
    if (!this.emailFrom.trim()) return;
    // The server verifies the relay by sending a real message to `to` before
    // accepting the configuration — at least one recipient is required.
    const recipients = this.emailTo
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    if (recipients.length === 0) {
      this.error.set('At least one test recipient is required.');
      return;
    }
    let body: TransactionalEmailSet;
    if (this.emailKind === 'smtp') {
      if (!this.smtpHost.trim()) return;
      body = {
        kind: 'smtp',
        smtp: {
          host: this.smtpHost.trim(),
          port: this.smtpPort,
          username: this.smtpUsername.trim() || undefined,
          password: this.smtpPassword || undefined,
          from: this.emailFrom.trim(),
          to: recipients,
          encryption: this.smtpEncryption,
        },
      };
    } else {
      if (!this.resendApiKey) return;
      body = {
        kind: 'resend',
        resend: { api_key: this.resendApiKey, from: this.emailFrom.trim(), to: recipients },
      };
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().setTransactionalEmail(body);
      this.smtpPassword = '';
      this.resendApiKey = '';
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async rotate(): Promise<void> {
    if (
      !confirm(
        'Rotate the encryption key? Every stored secret is re-encrypted in batches under the new key — it runs as a background job and is safe to run live.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      // 202: the re-encryption is a job — reload the status, which now carries
      // rotation_job_uuid, instead of waiting here.
      await this.api.client().rotateEncryption();
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- OAuth/OIDC providers ---------------------------------------------------

  protected oauthConfigOf(key: string): OauthProviderConfig | undefined {
    return this.oauthConfigs().find((c) => c.provider === key);
  }

  protected oauthLabel(key: string): string {
    return OAUTH_PROVIDERS.find((p) => p.key === key)?.label ?? key;
  }

  protected oauthNeedsIssuer(key: string): boolean {
    return OAUTH_PROVIDERS.find((p) => p.key === key)?.needsIssuer ?? false;
  }

  protected callbackHint(): string {
    return `${window.location.origin}/auth/oauth/{provider}/callback`;
  }

  protected editOauth(key: string): void {
    const existing = this.oauthConfigOf(key);
    this.oauthClientId = existing?.client_id ?? '';
    this.oauthClientSecret = ''; // write-only: always re-entered
    this.oauthIssuer = existing?.issuer_url ?? '';
    this.oauthDisplayName = existing?.display_name ?? '';
    this.oauthEnabled = existing?.enabled ?? true;
    this.oauthEditing.set(key);
  }

  protected async saveOauth(): Promise<void> {
    const key = this.oauthEditing();
    if (!key || !this.oauthClientId.trim() || !this.oauthClientSecret) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().setOauthProvider(key, {
        client_id: this.oauthClientId.trim(),
        client_secret: this.oauthClientSecret,
        issuer_url: this.oauthNeedsIssuer(key) ? this.oauthIssuer.trim() : undefined,
        display_name: this.oauthDisplayName.trim() || undefined,
        enabled: this.oauthEnabled,
      });
      this.oauthEditing.set(null);
      this.oauthClientSecret = '';
      await this.loadOauth();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeOauth(key: string): Promise<void> {
    if (
      !confirm(
        `Remove the ${this.oauthLabel(key)} sign-in? Linked accounts keep their other credentials.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteOauthProvider(key);
      await this.loadOauth();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async loadOauth(): Promise<void> {
    const res = await this.api.client().listOauthProviders();
    this.oauthConfigs.set(res.data);
  }

  // toggleApi flips the public-API gate. Turning it off is confirmed; the
  // dashboard stays reachable (its session is exempt), so this is recoverable.
  protected async toggleApi(next: boolean): Promise<void> {
    if (!next && !confirm(
      'Disable the public API? Token, script and CI calls are refused immediately. ' +
        'The dashboard keeps working, so you can re-enable it here.',
    )) {
      this.apiOn = true; // revert the switch, user declined
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      const state = next ? await this.api.client().enableApi() : await this.api.client().disableApi();
      this.apiOn = state.api_enabled;
    } catch (err) {
      this.apiOn = !next; // revert on failure
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
