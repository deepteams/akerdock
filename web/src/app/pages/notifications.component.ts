import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
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
  imports: [
    FormsModule,
    RouterLink,
    SlicePipe,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Notifications</h1>
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="creating.set(!creating())">
          <akd-icon name="plus" [size]="15" />
          {{ creating() ? 'Cancel' : 'New channel' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (testResult(); as result) {
        <div
          class="akd-toast toast"
          [class.akd-toast--ok]="result.delivered"
          [class.akd-toast--danger]="!result.delivered"
          role="status"
        >
          <span class="akd-toast__icon">
            <akd-icon [name]="result.delivered ? 'check-circle' : 'circle-x'" [size]="16" />
          </span>
          <div class="toast__body">
            <div class="akd-toast__title">
              {{
                result.delivered
                  ? 'Test sent to ' + result.channel
                  : 'Test to ' + result.channel + ' failed'
              }}
            </div>
            @if (!result.delivered) {
              <div class="akd-toast__msg">{{ result.error ?? 'unknown reason' }}</div>
            }
          </div>
          <button
            class="akd-iconbtn"
            type="button"
            aria-label="Dismiss test result"
            (click)="testResult.set(null)"
          >
            <akd-icon name="x" [size]="14" />
          </button>
        </div>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="nc-kind">Type</label>
              <div class="akd-select">
                <select id="nc-kind" name="kind" class="akd-input" [(ngModel)]="kind">
                  @for (k of kinds; track k) {
                    <option [value]="k">{{ k }}</option>
                  }
                </select>
              </div>
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="nc-name">Name</label>
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
                  <label class="akd-field__label" for="nc-host">Host</label>
                  <input
                    id="nc-host"
                    name="host"
                    class="akd-input"
                    [(ngModel)]="smtpHost"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-port">Port</label>
                  <input
                    id="nc-port"
                    name="port"
                    type="number"
                    class="akd-input"
                    [(ngModel)]="smtpPort"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-enc">Encryption</label>
                  <div class="akd-select">
                    <select
                      id="nc-enc"
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
                  <label class="akd-field__label" for="nc-user">Username (optional)</label>
                  <input
                    id="nc-user"
                    name="username"
                    class="akd-input"
                    [(ngModel)]="smtpUsername"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-pass">Password (optional)</label>
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
                  <label class="akd-field__label" for="nc-from">From</label>
                  <input id="nc-from" name="from" class="akd-input" [(ngModel)]="from" required />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-to">To (comma-separated)</label>
                  <input id="nc-to" name="to" class="akd-input" [(ngModel)]="to" required />
                </div>
              </div>
            }
            @case ('resend') {
              <div class="row">
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-key">API key</label>
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
                  <label class="akd-field__label" for="nc-from">From</label>
                  <input id="nc-from" name="from" class="akd-input" [(ngModel)]="from" required />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-to">To (comma-separated)</label>
                  <input id="nc-to" name="to" class="akd-input" [(ngModel)]="to" required />
                </div>
              </div>
            }
            @case ('telegram') {
              <div class="row">
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-bot">Bot token</label>
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
                  <label class="akd-field__label" for="nc-chat">Chat id</label>
                  <input
                    id="nc-chat"
                    name="chat_id"
                    class="akd-input"
                    [(ngModel)]="telegramChatId"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-topic">Topic id (optional)</label>
                  <input
                    id="nc-topic"
                    name="topic_id"
                    class="akd-input"
                    [(ngModel)]="telegramTopicId"
                  />
                </div>
              </div>
            }
            @case ('pushover') {
              <div class="row">
                <div class="akd-field">
                  <label class="akd-field__label" for="nc-token">Application token</label>
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
                  <label class="akd-field__label" for="nc-userkey">User key</label>
                  <input
                    id="nc-userkey"
                    name="user_key"
                    class="akd-input"
                    [(ngModel)]="pushoverUserKey"
                    required
                  />
                </div>
              </div>
            }
            @default {
              <div class="akd-field">
                <label class="akd-field__label" for="nc-url">Webhook URL</label>
                <input
                  id="nc-url"
                  name="url"
                  class="akd-input akd-input--mono"
                  placeholder="https://hooks.slack.com/…"
                  [(ngModel)]="url"
                  [disabled]="busy()"
                  required
                />
              </div>
            }
          }

          <p class="akd-field__hint">
            Secrets (URL, tokens, passwords) are write-only: encrypted at rest and never returned by
            the API.
          </p>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              {{ busy() ? 'Adding…' : 'Add channel' }}
            </button>
          </div>
        </form>
      }

      <akd-card title="Channels" [padded]="false">
        @if (loading()) {
          <p class="akd-muted pad">Loading…</p>
        } @else if (channels().length === 0) {
          <akd-empty-state
            icon="bell"
            title="No notification channels yet"
            message="Add one so failures reach a human before a user does."
          />
        } @else {
          <table class="akd-table">
            <caption class="sr-only">
              Notification channels of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Channel</th>
                <th scope="col">Transport</th>
                <th scope="col">State</th>
                <th scope="col">Created</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (channel of channels(); track channel.uuid) {
                <tr>
                  <td class="akd-mono">
                    <a [routerLink]="['/notifications', channel.uuid]">{{ channel.name }}</a>
                  </td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ channel.kind }}</span>
                  </td>
                  <td>
                    <akd-status-badge
                      domain="resource"
                      [state]="channel.enabled ? 'ready' : 'stopped'"
                      [label]="channel.enabled ? 'enabled' : 'disabled'"
                    />
                  </td>
                  <td class="akd-mono akd-muted">{{ channel.created_at | slice: 0 : 10 }}</td>
                  <td class="right">
                    <span class="actions">
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="test(channel)"
                      >
                        <akd-icon name="send" [size]="14" />
                        Send test
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="remove(channel)"
                        [attr.aria-label]="'Delete channel ' + channel.name"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </span>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </akd-card>
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .toast {
        margin-bottom: var(--space-5);
      }
      .toast__body {
        flex: 1;
      }
      .create {
        display: grid;
        gap: var(--space-3);
        margin-bottom: var(--space-5);
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
      .akd-field__hint {
        margin: 0;
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .actions {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
      }
      akd-card {
        display: block;
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
  protected readonly creating = signal(false);
  protected readonly testResult = signal<{
    channel: string;
    delivered: boolean;
    error?: string | null;
  } | null>(null);

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
      this.creating.set(false);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async test(channel: NotificationChannel): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.testResult.set(null);
    try {
      const result = await this.api.client().testNotificationChannel(channel.uuid);
      this.testResult.set({ channel: channel.name, ...result });
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
