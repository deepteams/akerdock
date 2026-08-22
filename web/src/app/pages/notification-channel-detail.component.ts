import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router } from '@angular/router';
import { ApiService } from '../core/api.service';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { BreadcrumbComponent, type Crumb } from '../../ui/breadcrumb/breadcrumb.component';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import type { components } from '../../api/schema';

type NotificationChannel = components['schemas']['NotificationChannel'];
type NotificationChannelUpdate = components['schemas']['NotificationChannelUpdate'];
type NotificationRule = components['schemas']['NotificationRule'];

type Severity = 'info' | 'warning' | 'critical';
type TabId = 'settings' | 'events' | 'routing';
interface CatalogEvent {
  type: string;
  label: string;
  severity: Severity;
}

/** The events a channel can subscribe to, grouped for the toggle grid. Types
 * match exactly what the backend emits (notify.SeverityOf + outbox `*.v1`) —
 * the dispatcher matches rules by exact event_type. Internal events
 * (test, digest, invitation, email test) are deliberately not listed. */
const EVENT_CATALOG: { title: string; events: CatalogEvent[] }[] = [
  {
    title: 'Deployments',
    events: [
      { type: 'deployment.succeeded.v1', label: 'Deployment succeeded', severity: 'info' },
      { type: 'deployment.failed.v1', label: 'Deployment failed', severity: 'critical' },
      { type: 'deployment.cancelled.v1', label: 'Deployment cancelled', severity: 'warning' },
      { type: 'application.created.v1', label: 'Application created', severity: 'info' },
    ],
  },
  {
    title: 'Previews',
    events: [
      { type: 'application.preview.created.v1', label: 'Preview created', severity: 'info' },
      { type: 'application.preview.updated.v1', label: 'Preview updated', severity: 'info' },
      {
        type: 'application.preview.expiring.v1',
        label: 'Preview expiring (TTL)',
        severity: 'warning',
      },
      { type: 'application.preview.deleted.v1', label: 'Preview deleted', severity: 'info' },
    ],
  },
  {
    title: 'Backups',
    events: [
      { type: 'backup.failed.v1', label: 'Backup failed', severity: 'critical' },
      { type: 'backup.partial.v1', label: 'Backup partial', severity: 'warning' },
      { type: 'backup.drill_failed.v1', label: 'Restore drill failed', severity: 'critical' },
    ],
  },
  {
    title: 'Servers',
    events: [
      { type: 'server.unreachable.v1', label: 'Server unreachable', severity: 'critical' },
      { type: 'server.updated.v1', label: 'Server updated', severity: 'info' },
      { type: 'server.cleanup.completed.v1', label: 'Docker cleanup completed', severity: 'info' },
      { type: 'server.cleanup.failed.v1', label: 'Docker cleanup failed', severity: 'critical' },
    ],
  },
  {
    title: 'Certificates & tasks',
    events: [
      { type: 'certificate.expiring.v1', label: 'Certificate expiring', severity: 'warning' },
      { type: 'scheduled_task.succeeded.v1', label: 'Scheduled task succeeded', severity: 'info' },
      { type: 'scheduled_task.failed.v1', label: 'Scheduled task failed', severity: 'critical' },
    ],
  },
  {
    title: 'Uptime & platform',
    events: [
      { type: 'uptime.check.failed.v1', label: 'Uptime check failed', severity: 'critical' },
      { type: 'uptime.check.recovered.v1', label: 'Uptime check recovered', severity: 'info' },
      { type: 'job.dead_letter.v1', label: 'Job dead-lettered', severity: 'critical' },
    ],
  },
];

@Component({
  selector: 'app-notification-channel-detail',
  standalone: true,
  imports: [FormsModule, BreadcrumbComponent, CardComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <akd-breadcrumb [items]="crumbs()" />

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (channel(); as ch) {
        <header class="akd-bar">
          <h1>{{ ch.name }}</h1>
          <span class="akd-badge akd-badge--mono">{{ ch.kind }}</span>
          <akd-status-badge
            domain="resource"
            [state]="ch.enabled ? 'ready' : 'stopped'"
            [label]="ch.enabled ? 'enabled' : 'disabled'"
          />
          <span class="grow"></span>
          <button
            class="akd-btn akd-btn--secondary"
            type="button"
            [disabled]="busy()"
            (click)="toggle(ch)"
          >
            {{ ch.enabled ? 'Disable' : 'Enable' }}
          </button>
          <button
            class="akd-btn akd-btn--ghost"
            type="button"
            [disabled]="busy()"
            (click)="test(ch)"
          >
            <akd-icon name="send" [size]="15" />
            Send test
          </button>
          <button
            class="akd-btn akd-btn--danger"
            type="button"
            [disabled]="busy()"
            (click)="remove(ch)"
          >
            <akd-icon name="trash-2" [size]="15" />
            Delete
          </button>
        </header>

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
                {{ result.delivered ? 'Test delivered.' : 'Test failed' }}
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

        <nav class="akd-tabs" role="tablist" aria-label="Channel sections">
          @for (t of tabs; track t.id) {
            <button
              type="button"
              class="akd-tab"
              role="tab"
              [class.akd-tab--active]="tab() === t.id"
              [attr.aria-selected]="tab() === t.id"
              (click)="selectTab(t.id)"
            >
              {{ t.label }}
              @if (t.id === 'routing' && rules().length > 0) {
                <span class="akd-tab__count">{{ rules().length }}</span>
              }
            </button>
          }
        </nav>

        @switch (tab()) {
          @case ('settings') {
            <akd-card title="Settings">
              <p class="akd-field__hint hint">
                Stored secrets are never returned, so replacing the configuration means re-entering
                it in full — blank means "leave the whole config as it is".
              </p>
              <form class="form" (ngSubmit)="save(ch)">
                <div class="akd-field">
                  <label class="akd-field__label" for="ch-name">Name</label>
                  <input
                    id="ch-name"
                    name="name"
                    class="akd-input"
                    [(ngModel)]="name"
                    [disabled]="busy()"
                    required
                  />
                </div>

                @switch (ch.kind) {
                  @case ('smtp') {
                    <div class="row">
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-host">Host</label>
                        <input id="ch-host" name="host" class="akd-input" [(ngModel)]="smtpHost" />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-port">Port</label>
                        <input
                          id="ch-port"
                          name="port"
                          type="number"
                          class="akd-input"
                          [(ngModel)]="smtpPort"
                        />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-enc">Encryption</label>
                        <div class="akd-select">
                          <select
                            id="ch-enc"
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
                        <label class="akd-field__label" for="ch-user">Username (optional)</label>
                        <input
                          id="ch-user"
                          name="username"
                          class="akd-input"
                          [(ngModel)]="smtpUsername"
                        />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-pass">Password (optional)</label>
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
                        <label class="akd-field__label" for="ch-from">From</label>
                        <input id="ch-from" name="from" class="akd-input" [(ngModel)]="from" />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-to">To (comma-separated)</label>
                        <input id="ch-to" name="to" class="akd-input" [(ngModel)]="to" />
                      </div>
                    </div>
                  }
                  @case ('resend') {
                    <div class="row">
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-key">API key</label>
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
                        <label class="akd-field__label" for="ch-from">From</label>
                        <input id="ch-from" name="from" class="akd-input" [(ngModel)]="from" />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-to">To (comma-separated)</label>
                        <input id="ch-to" name="to" class="akd-input" [(ngModel)]="to" />
                      </div>
                    </div>
                  }
                  @case ('telegram') {
                    <div class="row">
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-bot">Bot token</label>
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
                        <label class="akd-field__label" for="ch-chat">Chat id</label>
                        <input
                          id="ch-chat"
                          name="chat_id"
                          class="akd-input"
                          [(ngModel)]="telegramChatId"
                        />
                      </div>
                      <div class="akd-field">
                        <label class="akd-field__label" for="ch-topic">Topic id (optional)</label>
                        <input
                          id="ch-topic"
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
                        <label class="akd-field__label" for="ch-token">Application token</label>
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
                        <label class="akd-field__label" for="ch-userkey">User key</label>
                        <input
                          id="ch-userkey"
                          name="user_key"
                          class="akd-input"
                          [(ngModel)]="pushoverUserKey"
                        />
                      </div>
                    </div>
                  }
                  @default {
                    <div class="akd-field">
                      <label class="akd-field__label" for="ch-url">Webhook URL</label>
                      <input
                        id="ch-url"
                        name="url"
                        class="akd-input akd-input--mono"
                        placeholder="leave blank to keep the stored URL"
                        [(ngModel)]="url"
                      />
                    </div>
                  }
                }

                <div>
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    {{ busy() ? 'Saving…' : 'Save' }}
                  </button>
                </div>
              </form>
            </akd-card>
          }
          @case ('events') {
            <akd-card title="Events">
              <p class="akd-field__hint hint">
                Turn on the events this channel should receive (team-wide). For finer control — per
                project/environment, minimum severity, digests — use Advanced routing below.
              </p>
              @for (group of eventCatalog; track group.title) {
                <div class="evgroup">
                  <div class="evgroup__title">{{ group.title }}</div>
                  @for (ev of group.events; track ev.type) {
                    <label class="ev">
                      <input
                        type="checkbox"
                        class="akd-switch"
                        [checked]="isSubscribed(ev.type)"
                        [disabled]="busy()"
                        (change)="toggleEvent(ch, ev.type, $any($event.target).checked)"
                      />
                      <span class="ev__label">{{ ev.label }}</span>
                      <span class="akd-badge akd-badge--mono ev__sev sev-{{ ev.severity }}">{{
                        ev.severity
                      }}</span>
                      <code class="ev__type akd-muted">{{ ev.type }}</code>
                    </label>
                  }
                </div>
              }
            </akd-card>
          }
          @case ('routing') {
            <akd-card title="Advanced routing">
              <p class="akd-field__hint hint">
                A rule routes matching events to this channel — optionally scoped to a project or
                environment, above a minimum severity, batched into a digest. Critical events always
                bypass digests and debounce windows. The toggles above are simply team-wide rules.
              </p>

              <form class="form rule-form" (ngSubmit)="createRule(ch)">
                <div class="row">
                  <div class="akd-field">
                    <label class="akd-field__label" for="rl-event">Event type</label>
                    <input
                      id="rl-event"
                      name="event_type"
                      class="akd-input akd-input--mono"
                      placeholder="e.g. deployment.failed.v1"
                      [(ngModel)]="ruleEventType"
                      [disabled]="busy()"
                      required
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="rl-severity">Min severity</label>
                    <div class="akd-select">
                      <select
                        id="rl-severity"
                        name="min_severity"
                        class="akd-input"
                        [(ngModel)]="ruleMinSeverity"
                      >
                        <option value="info">info</option>
                        <option value="warning">warning</option>
                        <option value="critical">critical</option>
                      </select>
                    </div>
                  </div>
                </div>
                <div class="row">
                  <div class="akd-field">
                    <label class="akd-field__label" for="rl-project"
                      >Project uuid (blank = whole team)</label
                    >
                    <input
                      id="rl-project"
                      name="project_uuid"
                      class="akd-input akd-input--mono"
                      [(ngModel)]="ruleProjectUuid"
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="rl-env">Environment uuid (optional)</label>
                    <input
                      id="rl-env"
                      name="environment_uuid"
                      class="akd-input akd-input--mono"
                      [(ngModel)]="ruleEnvironmentUuid"
                    />
                  </div>
                </div>
                <div class="row digest">
                  <label class="akd-check">
                    <input type="checkbox" name="digest_enabled" [(ngModel)]="ruleDigestEnabled" />
                    Digest (batch non-critical events)
                  </label>
                  @if (ruleDigestEnabled) {
                    <div class="akd-field">
                      <label class="akd-field__label" for="rl-digest"
                        >Digest window (minutes)</label
                      >
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
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    {{ busy() ? 'Adding…' : 'Add rule' }}
                  </button>
                </div>
              </form>

              @if (rules().length === 0) {
                <p class="akd-muted">No rules yet — this channel receives nothing.</p>
              } @else {
                <div class="rules">
                  @for (rule of rules(); track rule.uuid) {
                    <div class="rule">
                      <!-- Rules have no update endpoint: the switch reflects the
                           stored state and cannot be flipped in place. -->
                      <input
                        type="checkbox"
                        class="akd-switch"
                        [checked]="rule.enabled"
                        disabled
                        [attr.aria-label]="
                          'Rule ' + rule.event_type + (rule.enabled ? ' enabled' : ' disabled')
                        "
                      />
                      <div class="rule__text">
                        <div class="rule__label akd-mono">{{ rule.event_type }}</div>
                        <div class="rule__desc">
                          min severity {{ rule.min_severity }} ·
                          {{ rule.project_uuid ?? 'whole team' }}
                          @if (rule.environment_uuid) {
                            / {{ rule.environment_uuid }}
                          }
                          ·
                          {{
                            rule.digest_enabled
                              ? 'digest every ' + rule.digest_interval_minutes + ' min'
                              : 'no digest'
                          }}
                        </div>
                      </div>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="removeRule(ch, rule)"
                        [attr.aria-label]="'Delete rule for ' + rule.event_type"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </div>
                  }
                </div>
              }
            </akd-card>
          }
        }
      }
    </div>
  `,
  styles: [
    `
      akd-breadcrumb {
        display: block;
        margin-bottom: var(--space-4);
      }
      .grow {
        flex: 1;
      }
      .toast {
        margin-bottom: var(--space-5);
      }
      .toast__body {
        flex: 1;
      }
      .cards {
        display: grid;
        gap: var(--space-5);
      }
      .hint {
        margin: 0 0 var(--space-4);
      }
      .evgroup + .evgroup {
        margin-top: var(--space-4);
      }
      .evgroup__title {
        font-size: var(--text-xs);
        text-transform: uppercase;
        letter-spacing: var(--tracking-wide);
        color: var(--text-3);
        margin-bottom: var(--space-2);
      }
      .ev {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-1) 0;
        cursor: pointer;
      }
      .ev__label {
        min-width: 12rem;
      }
      .ev__type {
        margin-left: auto;
        font-size: var(--text-xs);
      }
      .sev-critical {
        color: var(--danger, var(--text-1));
      }
      .sev-warning {
        color: var(--warning, var(--text-2));
      }
      .sev-info {
        color: var(--text-3);
      }
      .form {
        display: grid;
        gap: var(--space-3);
      }
      .rule-form {
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
      .row.digest {
        align-items: end;
      }
      .rules {
        display: grid;
        gap: var(--space-4);
      }
      .rule {
        display: flex;
        align-items: flex-start;
        gap: var(--space-3);
      }
      .rule .akd-switch {
        margin-top: 2px;
      }
      .rule__text {
        flex: 1;
        min-width: 0;
      }
      .rule__label {
        font-size: var(--text-md);
        color: var(--text-1);
        overflow-wrap: anywhere;
      }
      .rule__desc {
        font-size: var(--text-sm);
        color: var(--text-3);
        margin-top: 2px;
      }
    `,
  ],
})
export class NotificationChannelDetailComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);
  private readonly router = inject(Router);
  private readonly route = inject(ActivatedRoute);

  /** Bound by the router from the :uuid path parameter. */
  readonly uuid = input.required<string>();

  protected readonly tabs = [
    { id: 'settings', label: 'Settings' },
    { id: 'events', label: 'Events' },
    { id: 'routing', label: 'Advanced routing' },
  ] as const;
  protected readonly tab = signal<TabId>('settings');
  /** The active tab lives in the URL (?tab=…): a refresh keeps it, and
   * back/forward walk the tabs. */
  readonly tabParam = input<string | undefined>(undefined, { alias: 'tab' });

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.route,
      queryParams: { tab: id === 'settings' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  protected readonly channel = signal<NotificationChannel | null>(null);
  protected readonly rules = signal<NotificationRule[]>([]);
  protected readonly eventCatalog = EVENT_CATALOG;
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly testResult = signal<{ delivered: boolean; error?: string | null } | null>(
    null,
  );

  protected readonly crumbs = computed<Crumb[]>(() => {
    const ch = this.channel();
    return ch
      ? [{ label: 'Notifications', link: '/notifications' }, { label: ch.name }]
      : [{ label: 'Notifications', link: '/notifications' }];
  });

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
      // URL -> state: seeds the tab on load and follows back/forward.
      effect(() => {
        const wanted = this.tabParam();
        const valid = this.tabs.find((t) => t.id === wanted)?.id;
        this.tab.set(valid ?? 'settings');
      });
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

  /** A channel receives an event when a TEAM-WIDE rule (no project/environment
   * scope) exists for it — that is what the toggles create and remove. Scoped
   * rules from Advanced routing are independent and do not flip a toggle. */
  protected isSubscribed(eventType: string): boolean {
    return this.rules().some(
      (r) => r.event_type === eventType && !r.project_uuid && !r.environment_uuid && r.enabled,
    );
  }

  /** Toggle a whole event on/off for this channel by creating or deleting its
   * team-wide rule. */
  protected async toggleEvent(
    ch: NotificationChannel,
    eventType: string,
    on: boolean,
  ): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      if (on) {
        await this.api.client().createNotificationRule(ch.uuid, {
          event_type: eventType,
          enabled: true,
          min_severity: 'info',
          project_uuid: null,
          environment_uuid: null,
          debounce_seconds: 0,
          digest_enabled: false,
          digest_interval_minutes: 60,
        });
      } else {
        const targets = this.rules().filter(
          (r) => r.event_type === eventType && !r.project_uuid && !r.environment_uuid,
        );
        for (const r of targets) {
          await this.api.client().deleteNotificationRule(ch.uuid, r.uuid);
        }
      }
      await this.load(ch.uuid);
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
    if (
      !(await this.confirm.ask({
        title: 'Delete the rule',
        message: `Delete the rule for "${rule.event_type}"?`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
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
    if (
      !(await this.confirm.ask({
        title: 'Delete the channel',
        message: `Delete the channel "${ch.name}"? Its rules go with it.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
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
