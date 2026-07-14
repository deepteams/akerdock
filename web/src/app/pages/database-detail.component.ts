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
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { TerminalComponent } from '../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../ui/terminal/protocol';
import { ApiService } from '../core/api.service';
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
  imports: [FormsModule, RouterLink, StatusBadgeComponent, TerminalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div>
          <a routerLink="/databases" class="back">← Databases</a>
          <h1>{{ database()?.name ?? '…' }}</h1>
        </div>
        @if (database(); as db) {
          <div class="actions">
            <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="lifecycle('restart')">
              Restart
            </button>
            @if (db.desired_status === 'stopped') {
              <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="lifecycle('start')">
                Start
              </button>
            } @else {
              <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="lifecycle('stop')">
                Stop
              </button>
            }
          </div>
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
        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Overview</h2>
            <span class="badges">
              <akd-status-badge domain="resource" [state]="db.desired_status" label="desired: {{ db.desired_status }}" />
              <akd-status-badge domain="resource" [state]="db.observed_status" label="observed: {{ db.observed_status }}" />
            </span>
          </header>
          @if (db.restart_required) {
            <p class="akd-error" role="alert">
              A configuration change is waiting for a restart to take effect.
            </p>
          }
          <dl class="akd-dl">
            <dt>Engine</dt>
            <dd>{{ db.engine }} ({{ db.image ?? 'default image' }})</dd>
            <dt>User / database</dt>
            <dd class="akd-mono">{{ db.postgres_user ?? '—' }} / {{ db.postgres_db ?? '—' }}</dd>
            <dt>Password</dt>
            <!-- Credentials come back null without read:sensitive (INV-003):
                 the page displays what the API returned, nothing more. -->
            <dd class="akd-mono">
              {{ db.is_redacted ? '(redacted — needs read:sensitive)' : (db.postgres_password ?? '—') }}
            </dd>
            <dt>Internal URL</dt>
            <dd class="akd-mono">
              {{ db.is_redacted ? '(redacted — needs read:sensitive)' : (db.internal_url ?? '—') }}
            </dd>
            <dt>External URL</dt>
            <dd class="akd-mono">
              {{
                db.is_public
                  ? db.is_redacted
                    ? '(redacted — needs read:sensitive)'
                    : (db.external_url ?? '—')
                  : 'not public'
              }}
            </dd>
            <dt>SSL</dt>
            <dd>{{ db.ssl_enabled ? 'enabled (' + (db.ssl_mode ?? 'disable') + ')' : 'disabled' }}</dd>
          </dl>
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Settings</h2>
          </header>
          <form class="form" (ngSubmit)="save()">
            <div class="akd-field">
              <label for="dbd-name">Name</label>
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
              <label for="dbd-description">Description</label>
              <input
                id="dbd-description"
                name="description"
                class="akd-input"
                [(ngModel)]="description"
                [disabled]="busy()"
              />
            </div>
            <label class="check">
              <input type="checkbox" name="isPublic" [(ngModel)]="isPublic" [disabled]="busy()" />
              Publicly reachable
            </label>
            @if (isPublic) {
              <div class="akd-field">
                <label for="dbd-port">Public port (empty = assigned automatically)</label>
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
              <button class="akd-btn" type="submit" [disabled]="busy() || !name.trim()">
                Save settings
              </button>
            </div>
          </form>
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Backups</h2>
          </header>

          <form class="form" (ngSubmit)="createPlan()">
            <div class="row">
              <div class="akd-field grow">
                <label for="bp-frequency">Frequency (cron or alias: daily, hourly…)</label>
                <input
                  id="bp-frequency"
                  name="frequency"
                  class="akd-input akd-mono"
                  placeholder="daily"
                  [(ngModel)]="planFrequency"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="bp-retention-count">Keep at most (0 = unlimited)</label>
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
                <label for="bp-retention-days">Max age, days (0 = unlimited)</label>
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
              <div class="akd-field grow">
                <label for="bp-s3">Upload to S3 (optional)</label>
                <select
                  id="bp-s3"
                  name="s3"
                  class="akd-select"
                  [(ngModel)]="planS3Uuid"
                  [disabled]="busy()"
                >
                  <option value="">Local only</option>
                  @for (s3 of s3Storages(); track s3.uuid) {
                    <option [value]="s3.uuid">{{ s3.name }} ({{ s3.bucket }})</option>
                  }
                </select>
              </div>
              <button class="akd-btn" type="submit" [disabled]="busy() || !planFrequency.trim()">
                Add plan
              </button>
            </div>
          </form>

          @if (plans().length === 0) {
            <div class="akd-empty">
              <p><strong>No backup plan.</strong></p>
              <p>A database without a backup plan is one incident away from being a memory.</p>
            </div>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">Backup plans of this database</caption>
              <thead>
                <tr>
                  <th scope="col">Frequency</th>
                  <th scope="col">Enabled</th>
                  <th scope="col">Last run</th>
                  <th scope="col">Next run</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (plan of plans(); track plan.uuid) {
                  <tr>
                    <td class="akd-mono">{{ plan.frequency }}</td>
                    <td class="akd-muted">{{ plan.enabled ? 'yes' : 'no' }}</td>
                    <td>
                      @if (plan.last_execution_status; as status) {
                        <akd-status-badge domain="task" [state]="status" />
                      } @else {
                        <span class="akd-muted">never</span>
                      }
                    </td>
                    <td class="akd-muted">{{ plan.next_run_at ?? '—' }}</td>
                    <td class="right">
                      <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="backupNow(plan)">
                        Backup now
                      </button>
                      <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="drillNow(plan)">
                        Run drill
                      </button>
                      <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="startEditPlan(plan)">
                        Edit
                      </button>
                      <button
                        class="akd-btn-ghost"
                        type="button"
                        [attr.aria-expanded]="expandedPlan() === plan.uuid"
                        (click)="toggleHistory(plan)"
                      >
                        {{ expandedPlan() === plan.uuid ? 'Hide history' : 'History' }}
                      </button>
                      <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="removePlan(plan)">
                        Delete
                      </button>
                    </td>
                  </tr>
                  @if (editingPlan() === plan.uuid) {
                    <tr>
                      <td colspan="5">
                        <form class="row" (ngSubmit)="saveEditPlan(plan)">
                          <div class="akd-field grow">
                            <label [for]="'bpe-frequency-' + plan.uuid">Frequency</label>
                            <input
                              [id]="'bpe-frequency-' + plan.uuid"
                              name="editFrequency"
                              class="akd-input akd-mono"
                              [(ngModel)]="editFrequency"
                              [disabled]="busy()"
                            />
                          </div>
                          <label class="check">
                            <input
                              type="checkbox"
                              name="editEnabled"
                              [(ngModel)]="editEnabled"
                              [disabled]="busy()"
                            />
                            Enabled
                          </label>
                          <button class="akd-btn" type="submit" [disabled]="busy()">Save</button>
                          <button class="akd-btn-ghost" type="button" (click)="editingPlan.set(null)">
                            Cancel
                          </button>
                        </form>
                      </td>
                    </tr>
                  }
                  @if (expandedPlan() === plan.uuid) {
                    <tr>
                      <td colspan="5">
                        <h3>Executions</h3>
                        @if (executions().length === 0) {
                          <p class="akd-muted">No execution yet.</p>
                        } @else {
                          <table class="akd-table">
                            <caption class="sr-only">Backup executions of this plan</caption>
                            <thead>
                              <tr>
                                <th scope="col">Status</th>
                                <th scope="col">File</th>
                                <th scope="col">Size</th>
                                <th scope="col">Checksum</th>
                                <th scope="col"><span class="sr-only">Actions</span></th>
                              </tr>
                            </thead>
                            <tbody>
                              @for (exec of executions(); track exec.uuid) {
                                <tr>
                                  <td>
                                    <akd-status-badge domain="task" [state]="exec.status" />
                                    @if (exec.message) {
                                      <span class="akd-muted"> {{ exec.message }}</span>
                                    }
                                  </td>
                                  <td class="akd-mono">{{ exec.filename ?? '—' }}</td>
                                  <td class="akd-muted">{{ size(exec.size_bytes) }}</td>
                                  <td class="akd-mono checksum">{{ exec.checksum ?? '—' }}</td>
                                  <td class="right">
                                    @if (exec.status === 'succeeded' || exec.status === 'partial') {
                                      <button
                                        class="akd-btn-danger"
                                        type="button"
                                        [disabled]="busy()"
                                        (click)="restore(plan, exec)"
                                      >
                                        Restore
                                      </button>
                                    }
                                  </td>
                                </tr>
                              }
                            </tbody>
                          </table>
                        }

                        <h3>Restore drills</h3>
                        @if (drills().length === 0) {
                          <p class="akd-muted">
                            No drill yet. A backup never restored is not a backup, it is a file.
                          </p>
                        } @else {
                          <table class="akd-table">
                            <caption class="sr-only">Restore drills of this plan</caption>
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
                                  <td class="akd-muted">{{ drill.started_at }}</td>
                                </tr>
                              }
                            </tbody>
                          </table>
                        }
                      </td>
                    </tr>
                  }
                }
              </tbody>
            </table>
          }
        </section>

        <section class="akd-card section">
          <akd-terminal
            title="Database shell"
            hint="Opens a shell in the database container — psql is available there. Commands you run touch live data."
            [open]="openTerminal"
          />
        </section>

        <section class="akd-card section danger-zone">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Delete this database</h2>
          </header>
          <label class="check">
            <input
              type="checkbox"
              name="deleteVolumes"
              [(ngModel)]="deleteVolumes"
              [disabled]="busy()"
            />
            Also delete its volumes — all stored data is destroyed with them
          </label>
          <div>
            <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove()">
              Delete database
            </button>
          </div>
        </section>
      }
    </div>
  `,
  styles: [
    `
      .back {
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
      }
      .back:hover {
        text-decoration: underline;
      }
      .akd-bar h1 {
        margin-top: var(--akd-space-1);
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .badges {
        display: inline-flex;
        gap: var(--akd-space-2);
      }
      .section {
        margin-bottom: var(--akd-space-5);
      }
      .form {
        display: grid;
        gap: var(--akd-space-3);
        max-width: 44rem;
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--akd-space-2);
        flex-wrap: wrap;
      }
      .row .grow {
        flex: 1;
        min-width: 12rem;
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .checksum {
        max-width: 16rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      h3 {
        margin: var(--akd-space-3) 0 var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .danger-zone {
        border-color: var(--akd-status-danger-fg);
      }
    `,
  ],
})
export class DatabaseDetailComponent {
  /** Bound from the route (`databases/:uuid`) by withComponentInputBinding. */
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

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

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const [db, plans, s3] = await Promise.all([
        this.api.client().getDatabase(uuid),
        this.api.client().listBackupPlans(uuid, { limit: 50 }),
        this.api.client().listS3Storages({ limit: 100 }),
      ]);
      this.setDatabase(db);
      this.plans.set(plans.data);
      this.s3Storages.set(s3.data);
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
      !confirm(
        `Delete this backup plan (${plan.frequency})? Scheduled backups stop; existing backup files follow the retention policy.`,
      )
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
      !confirm(
        `Restore the backup "${exec.filename ?? exec.uuid}" INTO THE LIVE DATABASE? Everything currently in it is overwritten by the backup's contents. This cannot be undone.`,
      )
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
    if (!confirm(`Delete the database "${db.name}"? The container is removed.${volumes}`)) {
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
