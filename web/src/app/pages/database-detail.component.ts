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
import { Router } from '@angular/router';
import { BreadcrumbComponent } from '../../ui/breadcrumb/breadcrumb.component';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { TerminalComponent } from '../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../ui/terminal/protocol';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiError } from '../../api/client';
import type { components } from '../../api/schema';

type Database = components['schemas']['Database'];
type BackupPlan = components['schemas']['BackupPlan'];
type BackupExecution = components['schemas']['BackupExecution'];
type RestoreDrill = components['schemas']['RestoreDrill'];
type S3Storage = components['schemas']['S3Storage'];

@Component({
  selector: 'app-database-detail',
  standalone: true,
  imports: [
    FormsModule,
    BreadcrumbComponent,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
    StatusBadgeComponent,
    TerminalComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <akd-breadcrumb [items]="crumbs()" />
      <header class="akd-bar">
        <h1>{{ database()?.name ?? '…' }}</h1>
        @if (database(); as db) {
          <akd-status-badge
            domain="resource"
            [state]="db.desired_status"
            label="desired: {{ db.desired_status }}"
          />
          <akd-status-badge
            domain="resource"
            [state]="db.observed_status"
            label="observed: {{ db.observed_status }}"
          />
          @if (db.image) {
            <span class="akd-badge akd-badge--accent akd-badge--mono">{{ db.image }}</span>
          }
          @if (db.ssl_enabled) {
            <span class="akd-badge akd-badge--mono">sslmode={{ db.ssl_mode ?? 'disable' }}</span>
          }
          <span class="grow"></span>
          <button
            class="akd-btn akd-btn--ghost"
            type="button"
            [disabled]="busy()"
            (click)="lifecycle('restart')"
          >
            <akd-icon name="rotate-cw" [size]="15" />
            Restart
          </button>
          @if (db.desired_status === 'stopped') {
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              [disabled]="busy()"
              (click)="lifecycle('start')"
            >
              <akd-icon name="play" [size]="15" />
              Start
            </button>
          } @else {
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              [disabled]="busy()"
              (click)="lifecycle('stop')"
            >
              <akd-icon name="square" [size]="15" />
              Stop
            </button>
          }
        }
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }
      @if (notice(); as message) {
        <p class="akd-muted" role="status">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (database(); as db) {
        @if (db.restart_required) {
          <p class="akd-error" role="alert">
            A configuration change is waiting for a restart to take effect.
          </p>
        }

        <div class="grid">
          <akd-card title="Connection">
            <div class="conn-list">
              <div>
                <div class="akd-field__label conn-label">Internal URL</div>
                <div class="conn">
                  <code class="akd-mono conn__code" [class.conn__code--masked]="!reveal()">{{
                    connection(db, db.internal_url)
                  }}</code>
                  <button
                    class="akd-iconbtn akd-iconbtn--bordered"
                    type="button"
                    [disabled]="db.is_redacted"
                    (click)="reveal.set(!reveal())"
                    [attr.aria-label]="
                      reveal() ? 'Hide credentials' : 'Reveal credentials (read:sensitive)'
                    "
                  >
                    <akd-icon [name]="reveal() ? 'eye-off' : 'eye'" [size]="15" />
                  </button>
                  <button
                    class="akd-iconbtn akd-iconbtn--bordered"
                    type="button"
                    [disabled]="db.is_redacted || !db.internal_url"
                    (click)="copy(db.internal_url)"
                    aria-label="Copy internal URL"
                  >
                    <akd-icon name="copy" [size]="15" />
                  </button>
                </div>
              </div>
              @if (db.is_public) {
                <div>
                  <div class="akd-field__label conn-label">External URL</div>
                  <div class="conn">
                    <code class="akd-mono conn__code" [class.conn__code--masked]="!reveal()">{{
                      connection(db, db.external_url)
                    }}</code>
                    <button
                      class="akd-iconbtn akd-iconbtn--bordered"
                      type="button"
                      [disabled]="db.is_redacted || !db.external_url"
                      (click)="copy(db.external_url)"
                      aria-label="Copy external URL"
                    >
                      <akd-icon name="copy" [size]="15" />
                    </button>
                  </div>
                </div>
              }
              <dl class="akd-dl">
                <dt>Engine</dt>
                <dd>{{ db.engine }} ({{ db.image ?? 'default image' }})</dd>
                <dt>User / database</dt>
                <dd class="akd-mono">
                  {{ db.postgres_user ?? '—' }} / {{ db.postgres_db ?? '—' }}
                </dd>
                <dt>Password</dt>
                <!-- Credentials come back null without read:sensitive (INV-003):
                     the page displays what the API returned, nothing more. -->
                <dd class="akd-mono">
                  {{
                    db.is_redacted
                      ? '(redacted — needs read:sensitive)'
                      : reveal()
                        ? (db.postgres_password ?? '—')
                        : '••••••••'
                  }}
                </dd>
                <dt>External URL</dt>
                <dd class="akd-mono">
                  @if (db.is_public) {
                    published on port {{ db.public_port ?? '?' }}
                  } @else {
                    not public
                  }
                </dd>
                <dt>SSL</dt>
                <dd>
                  {{ db.ssl_enabled ? 'enabled (' + (db.ssl_mode ?? 'disable') + ')' : 'disabled' }}
                </dd>
              </dl>
            </div>
          </akd-card>

          <akd-card title="Settings">
            <form class="form" (ngSubmit)="save()">
              <div class="akd-field">
                <label class="akd-field__label" for="dbd-name">Name</label>
                <input
                  id="dbd-name"
                  name="name"
                  class="akd-input"
                  required
                  [(ngModel)]="name"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="dbd-description">Description</label>
                <input
                  id="dbd-description"
                  name="description"
                  class="akd-input"
                  [(ngModel)]="description"
                  [disabled]="busy()"
                />
              </div>
              <label class="akd-check">
                <input type="checkbox" name="isPublic" [(ngModel)]="isPublic" [disabled]="busy()" />
                Publicly reachable
              </label>
              @if (isPublic) {
                <div class="akd-field">
                  <label class="akd-field__label" for="dbd-port">
                    Public port (empty = assigned automatically)
                  </label>
                  <input
                    id="dbd-port"
                    name="publicPort"
                    class="akd-input"
                    type="number"
                    min="1"
                    max="65535"
                    [(ngModel)]="publicPort"
                    [disabled]="busy()"
                  />
                </div>
              }
              <div>
                <button
                  class="akd-btn akd-btn--primary"
                  type="submit"
                  [disabled]="busy() || !name.trim()"
                >
                  Save settings
                </button>
              </div>
            </form>
          </akd-card>
        </div>

        <akd-card title="Backup plans" [padded]="false" class="section">
          <span card-actions class="akd-badge akd-badge--mono">{{ plans().length }}</span>

          <form class="row pad" (ngSubmit)="createPlan()">
            <div class="akd-field grow-field">
              <label class="akd-field__label" for="bp-frequency">
                Frequency (cron or alias: daily, hourly…)
              </label>
              <input
                id="bp-frequency"
                name="frequency"
                class="akd-input akd-input--mono"
                placeholder="daily"
                [(ngModel)]="planFrequency"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="bp-retention-count">
                Keep at most (0 = unlimited)
              </label>
              <input
                id="bp-retention-count"
                name="retentionCount"
                class="akd-input"
                type="number"
                min="0"
                [(ngModel)]="planRetentionCount"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="bp-retention-days">
                Max age, days (0 = unlimited)
              </label>
              <input
                id="bp-retention-days"
                name="retentionDays"
                class="akd-input"
                type="number"
                min="0"
                [(ngModel)]="planRetentionDays"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field grow-field">
              <label class="akd-field__label" for="bp-s3">Upload to S3 (optional)</label>
              <div class="akd-select">
                <select
                  id="bp-s3"
                  name="s3"
                  class="akd-input"
                  [(ngModel)]="planS3Uuid"
                  [disabled]="busy()"
                >
                  <option value="">Local only</option>
                  @for (s3 of s3Storages(); track s3.uuid) {
                    <option [value]="s3.uuid">{{ s3.name }} ({{ s3.bucket }})</option>
                  }
                </select>
              </div>
            </div>
            <button
              class="akd-btn akd-btn--primary"
              type="submit"
              [disabled]="busy() || !planFrequency.trim()"
            >
              <akd-icon name="plus" [size]="15" />
              Add plan
            </button>
          </form>

          @if (plans().length === 0) {
            <div class="pad">
              <akd-empty-state
                icon="archive"
                title="No backup plan"
                message="A database without a backup plan is one incident away from being a memory."
              />
            </div>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">
                Backup plans of this database
              </caption>
              <thead>
                <tr>
                  <th scope="col">Frequency</th>
                  <th scope="col">Enabled</th>
                  <th scope="col">Last run</th>
                  <th scope="col">Next run</th>
                  <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (plan of plans(); track plan.uuid) {
                  <tr>
                    <td class="akd-mono">{{ plan.frequency }}</td>
                    <td>
                      @if (plan.enabled) {
                        <span class="akd-badge akd-badge--ok">enabled</span>
                      } @else {
                        <span class="akd-badge">disabled</span>
                      }
                    </td>
                    <td>
                      @if (plan.last_execution_status; as status) {
                        <akd-status-badge domain="task" [state]="status" />
                      } @else {
                        <span class="akd-muted">never</span>
                      }
                    </td>
                    <td class="akd-muted akd-mono">{{ plan.next_run_at ?? '—' }}</td>
                    <td class="right">
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="backupNow(plan)"
                      >
                        <akd-icon name="archive" [size]="14" />
                        Backup now
                      </button>
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="drillNow(plan)"
                      >
                        Run drill
                      </button>
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="startEditPlan(plan)"
                      >
                        Edit
                      </button>
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [attr.aria-expanded]="expandedPlan() === plan.uuid"
                        (click)="toggleHistory(plan)"
                      >
                        {{ expandedPlan() === plan.uuid ? 'Hide history' : 'History' }}
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="removePlan(plan)"
                        aria-label="Delete backup plan"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </td>
                  </tr>
                  @if (editingPlan() === plan.uuid) {
                    <tr>
                      <td colspan="5">
                        <form class="row edit-row" (ngSubmit)="saveEditPlan(plan)">
                          <div class="akd-field grow-field">
                            <label class="akd-field__label" [for]="'bpe-frequency-' + plan.uuid">
                              Frequency
                            </label>
                            <input
                              [id]="'bpe-frequency-' + plan.uuid"
                              name="editFrequency"
                              class="akd-input akd-input--mono"
                              [(ngModel)]="editFrequency"
                              [disabled]="busy()"
                            />
                          </div>
                          <label class="akd-check">
                            <input
                              type="checkbox"
                              name="editEnabled"
                              [(ngModel)]="editEnabled"
                              [disabled]="busy()"
                            />
                            Enabled
                          </label>
                          <button
                            class="akd-btn akd-btn--primary akd-btn--sm"
                            type="submit"
                            [disabled]="busy()"
                          >
                            Save
                          </button>
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            (click)="editingPlan.set(null)"
                          >
                            Cancel
                          </button>
                        </form>
                      </td>
                    </tr>
                  }
                }
              </tbody>
            </table>
          }
        </akd-card>

        @if (expandedPlanObj(); as plan) {
          <akd-card title="Backups" [padded]="false" class="section">
            <span card-actions class="akd-badge akd-badge--mono">plan {{ plan.frequency }}</span>
            @if (executions().length === 0) {
              <p class="akd-muted pad">No execution yet.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  Backup executions of this plan
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Taken</th>
                    <th scope="col">Status</th>
                    <th scope="col">File</th>
                    <th scope="col">Size</th>
                    <th scope="col">Destination</th>
                    <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  @for (exec of executions(); track exec.uuid) {
                    <tr>
                      <td class="akd-mono akd-muted">{{ exec.created_at }}</td>
                      <td>
                        <akd-status-badge domain="task" [state]="exec.status" />
                        @if (exec.message) {
                          <span class="akd-muted"> {{ exec.message }}</span>
                        }
                      </td>
                      <td class="akd-mono file" [title]="exec.checksum ?? ''">
                        {{ exec.filename ?? '—' }}
                      </td>
                      <td class="akd-mono akd-muted">{{ size(exec.size_bytes) }}</td>
                      <td>
                        @if (destination(exec); as dest) {
                          <span class="akd-badge akd-badge--mono">{{ dest }}</span>
                        } @else {
                          <span class="akd-muted">—</span>
                        }
                      </td>
                      <td class="right">
                        @if (exec.status === 'succeeded' || exec.status === 'partial') {
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            [disabled]="busy()"
                            (click)="restore(plan, exec)"
                          >
                            <akd-icon name="archive-restore" [size]="14" />
                            Restore
                          </button>
                        }
                      </td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          </akd-card>

          <akd-card title="Restore drills" [padded]="false" class="section">
            @if (drills().length === 0) {
              <p class="akd-muted pad">
                No drill yet. A backup never restored is not a backup, it is a file.
              </p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  Restore drills of this plan
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Status</th>
                    <th scope="col">Tables expected</th>
                    <th scope="col">Tables restored</th>
                    <th scope="col">Started</th>
                  </tr>
                </thead>
                <tbody>
                  @for (drill of drills(); track drill.uuid) {
                    <tr>
                      <td>
                        <akd-status-badge domain="task" [state]="drill.status" />
                        @if (drill.error_message) {
                          <span class="akd-muted"> {{ drill.error_message }}</span>
                        }
                      </td>
                      <td class="akd-muted">{{ drill.tables_expected ?? '—' }}</td>
                      <td class="akd-muted">{{ drill.tables_restored ?? '—' }}</td>
                      <td class="akd-muted akd-mono">{{ drill.started_at }}</td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          </akd-card>
        }

        <section class="akd-card section">
          <akd-terminal
            title="Database shell"
            hint="Opens a shell in the database container — psql is available there. Commands you run touch live data."
            [open]="openTerminal"
          />
        </section>

        <section class="akd-card section danger-zone">
          <header class="akd-bar danger-zone__bar">
            <h2>Delete this database</h2>
          </header>
          <label class="akd-check">
            <input
              type="checkbox"
              name="deleteVolumes"
              [(ngModel)]="deleteVolumes"
              [disabled]="busy()"
            />
            Also delete its volumes — all stored data is destroyed with them
          </label>
          <div>
            <button
              class="akd-btn akd-btn--danger"
              type="button"
              [disabled]="busy()"
              (click)="remove()"
            >
              <akd-icon name="trash-2" [size]="15" />
              Delete database
            </button>
          </div>
        </section>
      }
    </div>
  `,
  styles: [
    `
      akd-breadcrumb {
        display: block;
        margin-bottom: var(--space-3);
      }
      header.akd-bar h1 {
        font-family: var(--font-mono);
      }
      .grow {
        flex: 1;
      }
      .grid {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: var(--space-5);
        align-items: start;
        margin-bottom: var(--space-5);
      }
      .section {
        display: block;
        margin-bottom: var(--space-5);
      }
      .conn-list {
        display: grid;
        gap: var(--space-3);
      }
      .conn-label {
        margin-bottom: var(--space-1);
      }
      .conn {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .conn__code {
        flex: 1;
        background: var(--bg-inset);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-2);
        padding: var(--space-2) var(--space-3);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        color: var(--text-1);
      }
      .conn__code--masked {
        color: var(--text-3);
      }
      .form {
        display: grid;
        gap: var(--space-3);
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .row .grow-field {
        flex: 1;
        min-width: 12rem;
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .row.pad {
        border-bottom: 1px solid var(--border-1);
      }
      .edit-row {
        padding: var(--space-3) 0;
      }
      .file {
        max-width: 16rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .danger-zone {
        border-color: var(--danger-border);
      }
      .danger-zone__bar {
        margin-bottom: var(--space-3);
      }
    `,
  ],
})
export class DatabaseDetailComponent {
  /** Bound from the route (`databases/:uuid`) by withComponentInputBinding. */
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);
  private readonly confirm = inject(ConfirmService);

  /** Shell in the database container (§5.7) — psql lives there. */
  protected readonly openTerminal = async (): Promise<TerminalSessionInfo> =>
    (await this.api
      .client()
      .createDatabaseTerminalSession(this.uuid())) as unknown as TerminalSessionInfo;

  protected readonly database = signal<Database | null>(null);
  protected readonly plans = signal<BackupPlan[]>([]);
  protected readonly s3Storages = signal<S3Storage[]>([]);
  protected readonly executions = signal<BackupExecution[]>([]);
  protected readonly drills = signal<RestoreDrill[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly expandedPlan = signal<string | null>(null);
  protected readonly editingPlan = signal<string | null>(null);
  /** Explicit reveal — credentials stay masked until the operator asks. */
  protected readonly reveal = signal(false);

  protected readonly crumbs = computed(() => [
    { label: 'Databases', link: '/databases' },
    { label: this.database()?.name ?? '…' },
  ]);

  protected readonly expandedPlanObj = computed(
    () => this.plans().find((plan) => plan.uuid === this.expandedPlan()) ?? null,
  );

  protected name = '';
  protected description = '';
  protected isPublic = false;
  protected publicPort: number | null = null;
  protected deleteVolumes = false;

  protected planFrequency = 'daily';
  protected planRetentionCount = 7;
  protected planRetentionDays = 0;
  protected planS3Uuid = '';
  protected editFrequency = '';
  protected editEnabled = true;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected size(bytes: number | null | undefined): string {
    if (bytes == null) return '—';
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
    if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
    return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GiB`;
  }

  /**
   * What the code block shows. The API already redacts without read:sensitive
   * (INV-003); on top of that the password stays masked until the eye is
   * clicked — a shoulder-surfer sees dots, not credentials.
   */
  protected connection(db: Database, url: string | null | undefined): string {
    if (db.is_redacted) return '(redacted — needs read:sensitive)';
    if (!url) return '—';
    return this.reveal() ? url : url.replace(/\/\/([^:@/]+):[^@/]*@/, '//$1:••••••••@');
  }

  protected destination(exec: BackupExecution): string | null {
    const parts: string[] = [];
    if (exec.local_available) parts.push('local');
    if (exec.s3_uploaded) parts.push('s3');
    return parts.length ? parts.join(' + ') : null;
  }

  protected async copy(url: string | null | undefined): Promise<void> {
    if (!url) return;
    try {
      await navigator.clipboard.writeText(url);
      this.notice.set('Connection string copied to clipboard.');
    } catch {
      this.error.set('Clipboard access was denied by the browser.');
    }
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const [db, plans, s3] = await Promise.all([
        this.api.client().getDatabase(uuid),
        this.api.client().listBackupPlans(uuid, { limit: 50 }),
        fetchAll((cursor) => this.api.client().listS3Storages({ limit: 100, cursor })),
      ]);
      this.setDatabase(db);
      this.plans.set(plans.data);
      this.s3Storages.set(s3);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private setDatabase(db: Database): void {
    this.database.set(db);
    this.name = db.name;
    this.description = db.description ?? '';
    this.isPublic = db.is_public ?? false;
    this.publicPort = db.public_port ?? null;
  }

  private async refresh(): Promise<void> {
    try {
      this.setDatabase(await this.api.client().getDatabase(this.uuid()));
    } catch {
      // A failed refresh must not wipe what is already on screen.
    }
  }

  protected async lifecycle(action: 'start' | 'stop' | 'restart'): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const client = this.api.client();
      const accepted =
        action === 'start'
          ? await client.startDatabase(this.uuid())
          : action === 'stop'
            ? await client.stopDatabase(this.uuid())
            : await client.restartDatabase(this.uuid());
      this.notice.set(`${action} queued (job ${accepted.job_uuid}).`);
      await this.refresh();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async save(): Promise<void> {
    const db = this.database();
    if (!db || this.busy() || !this.name.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateDatabase(this.uuid(), db.version, {
        name: this.name.trim(),
        description: this.description.trim() || null,
        is_public: this.isPublic,
        public_port: this.isPublic ? this.publicPort : null,
      });
      this.setDatabase(updated);
      this.notice.set('Settings saved.');
    } catch (err) {
      // A 409 version conflict means someone else changed the configuration
      // while this form was open: reload their version instead of clobbering it.
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.refresh();
        this.error.set(
          'Your edit raced a concurrent change: the latest configuration was reloaded. Re-apply your edit on top of it.',
        );
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }

  protected async createPlan(): Promise<void> {
    if (this.busy() || !this.planFrequency.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createBackupPlan(this.uuid(), {
        frequency: this.planFrequency.trim(),
        timezone: 'UTC',
        enabled: true,
        dump_all: false,
        databases_to_include: [],
        save_local: true,
        save_s3: !!this.planS3Uuid,
        s3_storage_uuid: this.planS3Uuid || null,
        s3_only: false,
        local_retention: {
          max_count: this.planRetentionCount,
          max_age_days: this.planRetentionDays,
          max_size_gb: 0,
        },
        timeout_seconds: 3600,
        drill_enabled: false,
        drill_interval_days: 7,
      });
      await this.reloadPlans();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async reloadPlans(): Promise<void> {
    const page = await this.api.client().listBackupPlans(this.uuid(), { limit: 50 });
    this.plans.set(page.data);
  }

  protected startEditPlan(plan: BackupPlan): void {
    this.editingPlan.set(plan.uuid);
    this.editFrequency = plan.frequency;
    this.editEnabled = plan.enabled;
  }

  protected async saveEditPlan(plan: BackupPlan): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateBackupPlan(this.uuid(), plan.uuid, plan.version, {
        frequency: this.editFrequency.trim(),
        enabled: this.editEnabled,
      });
      this.editingPlan.set(null);
      await this.reloadPlans();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removePlan(plan: BackupPlan): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the backup plan',
        message: `Delete this backup plan (${plan.frequency})? Scheduled backups stop; existing backup files follow the retention policy.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteBackupPlan(this.uuid(), plan.uuid);
      if (this.expandedPlan() === plan.uuid) this.expandedPlan.set(null);
      await this.reloadPlans();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async backupNow(plan: BackupPlan): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const accepted = await this.api.client().executeBackupPlan(this.uuid(), plan.uuid);
      this.notice.set(`Backup queued (job ${accepted.job_uuid}).`);
      if (this.expandedPlan() === plan.uuid) await this.loadHistory(plan.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async drillNow(plan: BackupPlan): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const accepted = await this.api.client().runRestoreDrill(this.uuid(), plan.uuid);
      this.notice.set(
        `Restore drill queued (job ${accepted.job_uuid}) — the last dump is restored into a throwaway database and verified.`,
      );
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleHistory(plan: BackupPlan): Promise<void> {
    if (this.expandedPlan() === plan.uuid) {
      this.expandedPlan.set(null);
      return;
    }
    this.expandedPlan.set(plan.uuid);
    this.executions.set([]);
    this.drills.set([]);
    await this.loadHistory(plan.uuid);
  }

  private async loadHistory(planUuid: string): Promise<void> {
    try {
      const [executions, drills] = await Promise.all([
        this.api.client().listBackupExecutions(this.uuid(), planUuid, { limit: 20 }),
        this.api.client().listRestoreDrills(this.uuid(), planUuid, { limit: 20 }),
      ]);
      this.executions.set(executions.data);
      this.drills.set(drills.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async restore(plan: BackupPlan, exec: BackupExecution): Promise<void> {
    // A restore OVERWRITES the live database — the strongest warning on this
    // page, and the contract still demands confirm=true in the body on top.
    if (
      !(await this.confirm.ask({
        title: 'Restore into the live database',
        message: `Restore the backup "${exec.filename ?? exec.uuid}" INTO THE LIVE DATABASE? Everything currently in it is overwritten by the backup's contents. This cannot be undone.`,
        confirmLabel: 'Restore',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const accepted = await this.api
        .client()
        .restoreBackupExecution(this.uuid(), plan.uuid, exec.uuid, {
          confirm: true,
          allow_non_empty: true,
          source: exec.local_available ? 'local' : 's3',
        });
      this.notice.set(`Restore queued (job ${accepted.job_uuid}).`);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    const db = this.database();
    if (!db) return;
    const volumes = this.deleteVolumes
      ? ' Its volumes and ALL STORED DATA are destroyed with it.'
      : ' Its volumes are kept.';
    if (
      !(await this.confirm.ask({
        title: 'Delete the database',
        message: `Delete the database "${db.name}"? The container is removed.${volumes}`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteDatabase(this.uuid(), { delete_volumes: this.deleteVolumes });
      await this.router.navigateByUrl('/databases');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }
}
