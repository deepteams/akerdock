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
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type NotificationChannel = components['schemas']['NotificationChannel'];
type NotificationChannelUpdate = components['schemas']['NotificationChannelUpdate'];
type NotificationRule = components['schemas']['NotificationRule'];

@Component({
  selector: 'app-notification-channel-detail',
  standalone: true,
  imports: [FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <p><a routerLink="/notifications" class="akd-muted">← Notification channels</a></p>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (channel(); as ch) {
        <header class="akd-bar">
          <h1>{{ ch.name }}</h1>
          <div class="head-actions">
            <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="toggle(ch)">
              {{ ch.enabled ? 'Disable' : 'Enable' }}
            </button>
            <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="test(ch)">
              Send a test
            </button>
            <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove(ch)">
              Delete
            </button>
          </div>
        </header>

        <dl class="akd-dl">
          <dt>Type</dt>
          <dd>{{ ch.kind }}</dd>
          <dt>Enabled</dt>
          <dd>{{ ch.enabled ? 'yes' : 'no' }}</dd>
        </dl>

        @if (testResult(); as result) {
          <p [class]="result.delivered ? 'akd-muted' : 'akd-error'" role="status">
            {{ result.delivered ? 'Test delivered.' : 'Test failed: ' + (result.error ?? 'unknown reason') }}
          </p>
        }

        <section class="akd-card">
          <h2>Settings</h2>
          <p class="akd-muted hint">
            Stored secrets are never returned, so replacing the configuration means re-entering it
            in full — blank means "leave the whole config as it is".
          </p>
          <form class="form" (ngSubmit)="save(ch)">
            <div class="akd-field">
              <label for="ch-name">Name</label>
              <input id="ch-name" name="name" class="akd-input" [(ngModel)]="name" [disabled]="busy()" required />
            </div>

            @switch (ch.kind) {
              @case ('smtp') {
                <div class="row">
                  <div class="akd-field">
                    <label for="ch-host">Host</label>
                    <input id="ch-host" name="host" class="akd-input" [(ngModel)]="smtpHost" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-port">Port</label>
                    <input id="ch-port" name="port" type="number" class="akd-input" [(ngModel)]="smtpPort" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-enc">Encryption</label>
                    <select id="ch-enc" name="encryption" class="akd-select" [(ngModel)]="smtpEncryption">
                      <option value="starttls">starttls</option>
                      <option value="tls">tls</option>
                      <option value="none">none (local relay only)</option>
                    </select>
                  </div>
                </div>
                <div class="row">
                  <div class="akd-field">
                    <label for="ch-user">Username (optional)</label>
                    <input id="ch-user" name="username" class="akd-input" [(ngModel)]="smtpUsername" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-pass">Password (optional)</label>
                    <input
                      id="ch-pass"
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
                    <label for="ch-from">From</label>
                    <input id="ch-from" name="from" class="akd-input" [(ngModel)]="from" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-to">To (comma-separated)</label>
                    <input id="ch-to" name="to" class="akd-input" [(ngModel)]="to" />
                  </div>
                </div>
              }
              @case ('resend') {
                <div class="row">
                  <div class="akd-field">
                    <label for="ch-key">API key</label>
                    <input
                      id="ch-key"
                      name="api_key"
                      type="password"
                      class="akd-input"
                      autocomplete="new-password"
                      [(ngModel)]="resendApiKey"
                    />
                  </div>
                  <div class="akd-field">
                    <label for="ch-from">From</label>
                    <input id="ch-from" name="from" class="akd-input" [(ngModel)]="from" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-to">To (comma-separated)</label>
                    <input id="ch-to" name="to" class="akd-input" [(ngModel)]="to" />
                  </div>
                </div>
              }
              @case ('telegram') {
                <div class="row">
                  <div class="akd-field">
                    <label for="ch-bot">Bot token</label>
                    <input
                      id="ch-bot"
                      name="bot_token"
                      type="password"
                      class="akd-input"
                      autocomplete="new-password"
                      [(ngModel)]="telegramBotToken"
                    />
                  </div>
                  <div class="akd-field">
                    <label for="ch-chat">Chat id</label>
                    <input id="ch-chat" name="chat_id" class="akd-input" [(ngModel)]="telegramChatId" />
                  </div>
                  <div class="akd-field">
                    <label for="ch-topic">Topic id (optional)</label>
                    <input id="ch-topic" name="topic_id" class="akd-input" [(ngModel)]="telegramTopicId" />
                  </div>
                </div>
              }
              @case ('pushover') {
                <div class="row">
                  <div class="akd-field">
                    <label for="ch-token">Application token</label>
                    <input
                      id="ch-token"
                      name="token"
                      type="password"
                      class="akd-input"
                      autocomplete="new-password"
                      [(ngModel)]="pushoverToken"
                    />
                  </div>
                  <div class="akd-field">
                    <label for="ch-userkey">User key</label>
                    <input id="ch-userkey" name="user_key" class="akd-input" [(ngModel)]="pushoverUserKey" />
                  </div>
                </div>
              }
              @default {
                <div class="akd-field">
                  <label for="ch-url">Webhook URL</label>
                  <input
                    id="ch-url"
                    name="url"
                    class="akd-input"
                    placeholder="leave blank to keep the stored URL"
                    [(ngModel)]="url"
                  />
                </div>
              }
            }

            <div>
              <button class="akd-btn" type="submit" [disabled]="busy()">Save</button>
            </div>
          </form>
        </section>

        <section class="akd-card">
          <h2>Rules</h2>
          <p class="akd-muted hint">
            A rule routes matching events to this channel. Critical events always bypass digests
            and debounce windows.
          </p>

          <form class="form" (ngSubmit)="createRule(ch)">
            <div class="row">
              <div class="akd-field">
                <label for="rl-event">Event type</label>
                <input
                  id="rl-event"
                  name="event_type"
                  class="akd-input"
                  placeholder="e.g. deployment.failed.v1"
                  [(ngModel)]="ruleEventType"
                  [disabled]="busy()"
                  required
                />
              </div>
              <div class="akd-field">
                <label for="rl-severity">Min severity</label>
                <select id="rl-severity" name="min_severity" class="akd-select" [(ngModel)]="ruleMinSeverity">
                  <option value="info">info</option>
                  <option value="warning">warning</option>
                  <option value="critical">critical</option>
                </select>
              </div>
              <div class="akd-field">
                <label for="rl-project">Project uuid (blank = whole team)</label>
                <input id="rl-project" name="project_uuid" class="akd-input" [(ngModel)]="ruleProjectUuid" />
              </div>
              <div class="akd-field">
                <label for="rl-env">Environment uuid (optional)</label>
                <input id="rl-env" name="environment_uuid" class="akd-input" [(ngModel)]="ruleEnvironmentUuid" />
              </div>
            </div>
            <div class="row digest">
              <label class="check">
                <input type="checkbox" name="digest_enabled" [(ngModel)]="ruleDigestEnabled" />
                Digest (batch non-critical events)
              </label>
              @if (ruleDigestEnabled) {
                <div class="akd-field">
                  <label for="rl-digest">Digest window (minutes)</label>
                  <input
                    id="rl-digest"
                    name="digest_interval_minutes"
                    type="number"
                    min="1"
                    class="akd-input"
                    [(ngModel)]="ruleDigestInterval"
                  />
                </div>
              }
            </div>
            <div>
              <button class="akd-btn" type="submit" [disabled]="busy()">Add rule</button>
            </div>
          </form>

          @if (rules().length === 0) {
            <p class="akd-muted">No rules yet — this channel receives nothing.</p>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">Rules of this channel</caption>
              <thead>
                <tr>
                  <th scope="col">Event type</th>
                  <th scope="col">Min severity</th>
                  <th scope="col">Scope</th>
                  <th scope="col">Digest</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (rule of rules(); track rule.uuid) {
                  <tr>
                    <td class="akd-mono">{{ rule.event_type }}</td>
                    <td class="akd-muted">{{ rule.min_severity }}</td>
                    <td class="akd-muted">
                      {{ rule.project_uuid ?? 'whole team' }}
                      @if (rule.environment_uuid) {
                        / {{ rule.environment_uuid }}
                      }
                    </td>
                    <td class="akd-muted">
                      {{ rule.digest_enabled ? 'every ' + rule.digest_interval_minutes + ' min' : 'no' }}
                    </td>
                    <td class="right">
                      <button
                        class="akd-btn-danger"
                        type="button"
                        [disabled]="busy()"
                        (click)="removeRule(ch, rule)"
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </section>
      }
    </div>
  `,
  styles: [
    `
      .head-actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .akd-dl {
        margin-bottom: var(--akd-space-5);
      }
      .akd-card + .akd-card {
        margin-top: var(--akd-space-5);
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
      .row.digest {
        align-items: end;
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
    `,
  ],
})
export class NotificationChannelDetailComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  /** Bound by the router from the :uuid path parameter. */
  readonly uuid = input.required<string>();

  protected readonly channel = signal<NotificationChannel | null>(null);
  protected readonly rules = signal<NotificationRule[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly testResult = signal<{ delivered: boolean; error?: string | null } | null>(
    null,
  );

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

  protected ruleEventType = '';
  protected ruleMinSeverity: 'info' | 'warning' | 'critical' = 'info';
  protected ruleProjectUuid = '';
  protected ruleEnvironmentUuid = '';
  protected ruleDigestEnabled = false;
  protected ruleDigestInterval = 60;

  constructor() {
    // Router inputs are not set at construction time; the effect fires once
    // they are, and again if the route parameter changes in place.
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const client = this.api.client();
      const [channel, rules] = await Promise.all([
        client.getNotificationChannel(uuid),
        client.listNotificationRules(uuid),
      ]);
      this.channel.set(channel);
      this.rules.set(rules.data);
      this.name = channel.name;
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async toggle(ch: NotificationChannel): Promise<void> {
    await this.update(ch, { enabled: !ch.enabled });
  }

  /**
   * The config sub-object is only sent when the operator filled it in: the
   * stored secrets never come back, so an empty form must mean "unchanged",
   * not "erase".
   */
  protected async save(ch: NotificationChannel): Promise<void> {
    const body: NotificationChannelUpdate = {};
    if (this.name.trim() && this.name.trim() !== ch.name) body.name = this.name.trim();
    const recipients = this.to
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    switch (ch.kind) {
      case 'smtp':
        if (this.smtpHost.trim() && this.from.trim() && recipients.length > 0) {
          body.smtp = {
            host: this.smtpHost.trim(),
            port: this.smtpPort,
            username: this.smtpUsername.trim() || undefined,
            password: this.smtpPassword || undefined,
            from: this.from.trim(),
            to: recipients,
            encryption: this.smtpEncryption,
          };
        }
        break;
      case 'resend':
        if (this.resendApiKey && this.from.trim() && recipients.length > 0) {
          body.resend = { api_key: this.resendApiKey, from: this.from.trim(), to: recipients };
        }
        break;
      case 'telegram':
        if (this.telegramBotToken && this.telegramChatId.trim()) {
          body.telegram = {
            bot_token: this.telegramBotToken,
            chat_id: this.telegramChatId.trim(),
            topic_id: this.telegramTopicId.trim() || null,
          };
        }
        break;
      case 'pushover':
        if (this.pushoverToken && this.pushoverUserKey.trim()) {
          body.pushover = { token: this.pushoverToken, user_key: this.pushoverUserKey.trim() };
        }
        break;
      default:
        if (this.url.trim()) body.url = this.url.trim();
    }
    if (Object.keys(body).length === 0) {
      this.error.set('Nothing to save — fill in the fields to change.');
      return;
    }
    await this.update(ch, body);
  }

  private async update(ch: NotificationChannel, body: NotificationChannelUpdate): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateNotificationChannel(ch.uuid, ch.version, body);
      await this.load(ch.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async test(ch: NotificationChannel): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.testResult.set(null);
    try {
      this.testResult.set(await this.api.client().testNotificationChannel(ch.uuid));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async createRule(ch: NotificationChannel): Promise<void> {
    if (!this.ruleEventType.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createNotificationRule(ch.uuid, {
        event_type: this.ruleEventType.trim(),
        enabled: true,
        min_severity: this.ruleMinSeverity,
        project_uuid: this.ruleProjectUuid.trim() || null,
        environment_uuid: this.ruleEnvironmentUuid.trim() || null,
        debounce_seconds: 0,
        digest_enabled: this.ruleDigestEnabled,
        digest_interval_minutes: this.ruleDigestInterval,
      });
      this.ruleEventType = '';
      this.ruleProjectUuid = '';
      this.ruleEnvironmentUuid = '';
      await this.load(ch.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeRule(ch: NotificationChannel, rule: NotificationRule): Promise<void> {
    if (!confirm(`Delete the rule for "${rule.event_type}"?`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteNotificationRule(ch.uuid, rule.uuid);
      await this.load(ch.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(ch: NotificationChannel): Promise<void> {
    if (!confirm(`Delete the channel "${ch.name}"? Its rules go with it.`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteNotificationChannel(ch.uuid);
      await this.router.navigate(['/notifications']);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }
}
