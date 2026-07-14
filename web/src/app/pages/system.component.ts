import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type TransactionalEmail = components['schemas']['TransactionalEmail'];
type TransactionalEmailSet = components['schemas']['TransactionalEmailSet'];
type EncryptionStatus = components['schemas']['EncryptionStatus'];

@Component({
  selector: 'app-system',
  standalone: true,
  imports: [FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>System</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <h2>Transactional email</h2>
        @if (email(); as em) {
          <p class="akd-muted">
            @if (em.configured) {
              Configured — {{ em.kind }} sending as {{ em.from }}. While configured, invitation
              emails are sent automatically instead of handing out a link.
            } @else {
              Not configured. Invitations fall back to a one-time link you pass along yourself.
            }
          </p>
        }
        <form class="form" (ngSubmit)="saveEmail()">
          <div class="akd-field mode">
            <label for="em-kind">Mode</label>
            <select id="em-kind" name="kind" class="akd-select" [(ngModel)]="emailKind">
              <option value="smtp">smtp</option>
              <option value="resend">resend</option>
            </select>
          </div>
          @if (emailKind === 'smtp') {
            <div class="row">
              <div class="akd-field">
                <label for="em-host">Host</label>
                <input id="em-host" name="host" class="akd-input" [(ngModel)]="smtpHost" required />
              </div>
              <div class="akd-field">
                <label for="em-port">Port</label>
                <input id="em-port" name="port" type="number" class="akd-input" [(ngModel)]="smtpPort" />
              </div>
              <div class="akd-field">
                <label for="em-enc">Encryption</label>
                <select id="em-enc" name="encryption" class="akd-select" [(ngModel)]="smtpEncryption">
                  <option value="starttls">starttls</option>
                  <option value="tls">tls</option>
                  <option value="none">none (local relay only)</option>
                </select>
              </div>
            </div>
            <div class="row">
              <div class="akd-field">
                <label for="em-user">Username (optional)</label>
                <input id="em-user" name="username" class="akd-input" [(ngModel)]="smtpUsername" />
              </div>
              <div class="akd-field">
                <label for="em-pass">Password (optional)</label>
                <input
                  id="em-pass"
                  name="password"
                  type="password"
                  class="akd-input"
                  autocomplete="new-password"
                  [(ngModel)]="smtpPassword"
                />
              </div>
              <div class="akd-field">
                <label for="em-from">From</label>
                <input id="em-from" name="from" class="akd-input" [(ngModel)]="emailFrom" required />
              </div>
              <div class="akd-field">
                <label for="em-to">Test recipients (comma-separated)</label>
                <input id="em-to" name="to" class="akd-input" [(ngModel)]="emailTo" required />
              </div>
            </div>
          } @else {
            <div class="row">
              <div class="akd-field">
                <label for="em-key">API key</label>
                <input
                  id="em-key"
                  name="api_key"
                  type="password"
                  class="akd-input"
                  autocomplete="new-password"
                  [(ngModel)]="resendApiKey"
                  required
                />
              </div>
              <div class="akd-field">
                <label for="em-from">From</label>
                <input id="em-from" name="from" class="akd-input" [(ngModel)]="emailFrom" required />
              </div>
              <div class="akd-field">
                <label for="em-to">Test recipients (comma-separated)</label>
                <input id="em-to" name="to" class="akd-input" [(ngModel)]="emailTo" required />
              </div>
            </div>
          }
          <p class="akd-muted hint">
            Secrets (password, API key) are write-only: encrypted at rest, never returned by the
            API. Saving sends a verification message to the test recipients — an unreachable relay
            is refused on the spot.
          </p>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy()">Save configuration</button>
          </div>
        </form>
      </section>

      <section class="akd-card">
        <h2>Encryption</h2>
        @if (encryption(); as enc) {
          <dl class="akd-dl">
            <dt>Active key version</dt>
            <dd>{{ enc.active_key_version }}</dd>
            @if (enc.rotation_job_uuid; as jobUuid) {
              <dt>Rotation in progress</dt>
              <dd><a [routerLink]="['/jobs', jobUuid]" class="akd-mono">{{ jobUuid }}</a></dd>
            }
          </dl>
          @if (enc.key_versions.length > 0) {
            <table class="akd-table">
              <caption class="sr-only">Encrypted rows per key version</caption>
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
                        <span class="col">{{ col.table }}.{{ col.column }} ({{ col.rows }})</span>
                      } @empty {
                        —
                      }
                    </td>
                  </tr>
                }
              </tbody>
            </table>
            <p class="akd-muted hint">
              A rotation has converged when only the active version is still referenced.
            </p>
          }
        } @else {
          <p class="akd-muted">Loading…</p>
        }
        <div>
          <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="rotate()">
            Rotate encryption key
          </button>
        </div>
      </section>

      <section class="akd-card">
        <h2>API access</h2>
        @if (apiEnabled(); as state) {
          <p class="akd-muted" role="status">
            The API is now {{ state === 'enabled' ? 'enabled' : 'disabled' }}.
          </p>
        }
        <p class="akd-muted">
          Disabling refuses every API call immediately — tokens, scripts, CI, and this dashboard's
          own requests included. Only re-enabling stays reachable, from this page.
        </p>
        <div class="actions">
          <button class="akd-btn" type="button" [disabled]="busy()" (click)="enableApi()">
            Enable API
          </button>
          <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="disableApi()">
            Disable API
          </button>
        </div>
      </section>
    </div>
  `,
  styles: [
    `
      .akd-card {
        margin-bottom: var(--akd-space-5);
      }
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
        min-width: 180px;
      }
      .mode {
        max-width: 200px;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .col {
        display: block;
      }
    `,
  ],
})
export class SystemComponent {
  private readonly api = inject(ApiService);

  protected readonly email = signal<TransactionalEmail | null>(null);
  protected readonly encryption = signal<EncryptionStatus | null>(null);
  protected readonly apiEnabled = signal<'enabled' | 'disabled' | null>(null);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

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

  private async load(): Promise<void> {
    const client = this.api.client();
    try {
      const [email, encryption] = await Promise.all([
        client.getTransactionalEmail(),
        client.getEncryptionStatus(),
      ]);
      this.email.set(email);
      this.encryption.set(encryption);
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

  protected async enableApi(): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      const state = await this.api.client().enableApi();
      this.apiEnabled.set(state.api_enabled ? 'enabled' : 'disabled');
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async disableApi(): Promise<void> {
    // The gate covers the dashboard's own calls too (only /system/api/enable
    // is exempt) — the confirmation has to say so.
    if (
      !confirm(
        'Disable the API? Every call is refused immediately — scripts, CI and this dashboard included. Only the "Enable API" button on this page keeps working.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      const state = await this.api.client().disableApi();
      this.apiEnabled.set(state.api_enabled ? 'enabled' : 'disabled');
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
