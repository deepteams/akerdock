import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type NotificationChannel = components['schemas']['NotificationChannel'];
type ChannelKind = NotificationChannel['kind'];
type NotificationChannelCreate = components['schemas']['NotificationChannelCreate'];

const KINDS: ChannelKind[] = [
  'webhook',
  'slack',
  'discord',
  'smtp',
  'resend',
  'telegram',
  'pushover',
];

@Component({
  selector: 'app-notifications',
  standalone: true,
  imports: [FormsModule, RouterLink, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Notification channels</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <h2>Add a channel</h2>
        <form class="form" (ngSubmit)="create()">
          <div class="row">
            <div class="akd-field">
              <label for="nc-kind">Type</label>
              <select id="nc-kind" name="kind" class="akd-select" [(ngModel)]="kind">
                @for (k of kinds; track k) {
                  <option [value]="k">{{ k }}</option>
                }
              </select>
            </div>
            <div class="akd-field">
              <label for="nc-name">Name</label>
              <input
                id="nc-name"
                name="name"
                class="akd-input"
                placeholder="e.g. ops-alerts"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
          </div>

          <!-- Only the fields of the selected type: each kind has its own config
               shape in the contract, and the secrets in it are write-only. -->
          @switch (kind) {
            @case ('smtp') {
              <div class="row">
                <div class="akd-field">
                  <label for="nc-host">Host</label>
                  <input id="nc-host" name="host" class="akd-input" [(ngModel)]="smtpHost" required />
                </div>
                <div class="akd-field">
                  <label for="nc-port">Port</label>
                  <input id="nc-port" name="port" type="number" class="akd-input" [(ngModel)]="smtpPort" />
                </div>
                <div class="akd-field">
                  <label for="nc-enc">Encryption</label>
                  <select id="nc-enc" name="encryption" class="akd-select" [(ngModel)]="smtpEncryption">
                    <option value="starttls">starttls</option>
                    <option value="tls">tls</option>
                    <option value="none">none (local relay only)</option>
                  </select>
                </div>
              </div>
              <div class="row">
                <div class="akd-field">
                  <label for="nc-user">Username (optional)</label>
                  <input id="nc-user" name="username" class="akd-input" [(ngModel)]="smtpUsername" />
                </div>
                <div class="akd-field">
                  <label for="nc-pass">Password (optional)</label>
                  <input
                    id="nc-pass"
                    name="password"
                    type="password"
                    class="akd-input"
                    autocomplete="new-password"
                    [(ngModel)]="smtpPassword"
                  />
                </div>
              </div>
              <div class="row">
                <div class="akd-field">
                  <label for="nc-from">From</label>
                  <input id="nc-from" name="from" class="akd-input" [(ngModel)]="from" required />
                </div>
                <div class="akd-field">
                  <label for="nc-to">To (comma-separated)</label>
                  <input id="nc-to" name="to" class="akd-input" [(ngModel)]="to" required />
                </div>
              </div>
            }
            @case ('resend') {
              <div class="row">
                <div class="akd-field">
                  <label for="nc-key">API key</label>
                  <input
                    id="nc-key"
                    name="api_key"
                    type="password"
                    class="akd-input"
                    autocomplete="new-password"
                    [(ngModel)]="resendApiKey"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label for="nc-from">From</label>
                  <input id="nc-from" name="from" class="akd-input" [(ngModel)]="from" required />
                </div>
                <div class="akd-field">
                  <label for="nc-to">To (comma-separated)</label>
                  <input id="nc-to" name="to" class="akd-input" [(ngModel)]="to" required />
                </div>
              </div>
            }
            @case ('telegram') {
              <div class="row">
                <div class="akd-field">
                  <label for="nc-bot">Bot token</label>
                  <input
                    id="nc-bot"
                    name="bot_token"
                    type="password"
                    class="akd-input"
                    autocomplete="new-password"
                    [(ngModel)]="telegramBotToken"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label for="nc-chat">Chat id</label>
                  <input id="nc-chat" name="chat_id" class="akd-input" [(ngModel)]="telegramChatId" required />
                </div>
                <div class="akd-field">
                  <label for="nc-topic">Topic id (optional)</label>
                  <input id="nc-topic" name="topic_id" class="akd-input" [(ngModel)]="telegramTopicId" />
                </div>
              </div>
            }
            @case ('pushover') {
              <div class="row">
                <div class="akd-field">
                  <label for="nc-token">Application token</label>
                  <input
                    id="nc-token"
                    name="token"
                    type="password"
                    class="akd-input"
                    autocomplete="new-password"
                    [(ngModel)]="pushoverToken"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label for="nc-userkey">User key</label>
                  <input id="nc-userkey" name="user_key" class="akd-input" [(ngModel)]="pushoverUserKey" required />
                </div>
              </div>
            }
            @default {
              <div class="akd-field">
                <label for="nc-url">Webhook URL</label>
                <input
                  id="nc-url"
                  name="url"
                  class="akd-input"
                  placeholder="https://hooks.slack.com/…"
                  [(ngModel)]="url"
                  [disabled]="busy()"
                  required
                />
              </div>
            }
          }

          <p class="akd-muted hint">
            Secrets (URL, tokens, passwords) are write-only: encrypted at rest and never returned
            by the API.
          </p>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy()">Add channel</button>
          </div>
        </form>
      </section>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (channels().length === 0) {
        <div class="akd-empty">
          <p><strong>No notification channels yet.</strong></p>
          <p>Add one so failures reach a human before a user does.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Notification channels of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Type</th>
              <th scope="col">Enabled</th>
              <th scope="col">Created</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (channel of channels(); track channel.uuid) {
              <tr>
                <td>
                  <a [routerLink]="['/notifications', channel.uuid]">{{ channel.name }}</a>
                </td>
                <td class="akd-muted">{{ channel.kind }}</td>
                <td class="akd-muted">{{ channel.enabled ? 'yes' : 'no' }}</td>
                <td class="akd-muted">{{ channel.created_at | slice: 0 : 10 }}</td>
                <td class="right">
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(channel)"
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
        min-width: 180px;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
    `,
  ],
})
export class NotificationsComponent {
  private readonly api = inject(ApiService);

  protected readonly kinds = KINDS;
  protected readonly channels = signal<NotificationChannel[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected kind: ChannelKind = 'webhook';
  protected name = '';
  protected url = '';
  protected from = '';
  protected to = '';
  protected smtpHost = '';
  protected smtpPort = 587;
  protected smtpUsername = '';
  protected smtpPassword = '';
  protected smtpEncryption: 'starttls' | 'tls' | 'none' = 'starttls';
  protected resendApiKey = '';
  protected telegramBotToken = '';
  protected telegramChatId = '';
  protected telegramTopicId = '';
  protected pushoverToken = '';
  protected pushoverUserKey = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listNotificationChannels({ limit: 100 });
      this.channels.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private buildBody(): NotificationChannelCreate | null {
    const base: NotificationChannelCreate = {
      kind: this.kind,
      name: this.name.trim(),
      enabled: true,
    };
    const recipients = this.to
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    switch (this.kind) {
      case 'smtp':
        if (!this.smtpHost.trim() || !this.from.trim() || recipients.length === 0) return null;
        return {
          ...base,
          smtp: {
            host: this.smtpHost.trim(),
            port: this.smtpPort,
            username: this.smtpUsername.trim() || undefined,
            password: this.smtpPassword || undefined,
            from: this.from.trim(),
            to: recipients,
            encryption: this.smtpEncryption,
          },
        };
      case 'resend':
        if (!this.resendApiKey || !this.from.trim() || recipients.length === 0) return null;
        return {
          ...base,
          resend: { api_key: this.resendApiKey, from: this.from.trim(), to: recipients },
        };
      case 'telegram':
        if (!this.telegramBotToken || !this.telegramChatId.trim()) return null;
        return {
          ...base,
          telegram: {
            bot_token: this.telegramBotToken,
            chat_id: this.telegramChatId.trim(),
            topic_id: this.telegramTopicId.trim() || null,
          },
        };
      case 'pushover':
        if (!this.pushoverToken || !this.pushoverUserKey.trim()) return null;
        return {
          ...base,
          pushover: { token: this.pushoverToken, user_key: this.pushoverUserKey.trim() },
        };
      default:
        // webhook, slack, discord: a single webhook URL carries the config.
        if (!this.url.trim()) return null;
        return { ...base, url: this.url.trim() };
    }
  }

  protected async create(): Promise<void> {
    if (!this.name.trim()) return;
    const body = this.buildBody();
    if (!body) {
      this.error.set('Fill in the fields required by the selected type.');
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createNotificationChannel(body);
      this.name = '';
      this.url = '';
      this.smtpPassword = '';
      this.resendApiKey = '';
      this.telegramBotToken = '';
      this.pushoverToken = '';
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(channel: NotificationChannel): Promise<void> {
    if (!confirm(`Delete the channel "${channel.name}"? Its rules go with it.`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteNotificationChannel(channel.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
