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
import { StatusBadgeComponent } from '../../../ui/status-badge/status-badge.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type ScheduledTask = components['schemas']['ScheduledTask'];
type TaskExecution = components['schemas']['TaskExecution'];

@Component({
  selector: 'app-application-tasks-tab',
  standalone: true,
  imports: [FormsModule, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <form class="akd-card create" (ngSubmit)="create()">
      <div class="akd-field">
        <label for="tk-name">Name</label>
        <input
          id="tk-name"
          name="name"
          class="akd-input"
          required
          [(ngModel)]="name"
          [disabled]="busy()"
        />
      </div>
      <div class="akd-field">
        <label for="tk-command">Command (run in the container)</label>
        <input
          id="tk-command"
          name="command"
          class="akd-input akd-mono"
          placeholder="php artisan schedule:run"
          required
          [(ngModel)]="command"
          [disabled]="busy()"
        />
      </div>
      <div class="akd-field">
        <label for="tk-cron">Cron expression</label>
        <input
          id="tk-cron"
          name="cron"
          class="akd-input akd-mono"
          placeholder="0 3 * * *"
          required
          [(ngModel)]="cron"
          [disabled]="busy()"
        />
      </div>
      <div>
        <button class="akd-btn" type="submit" [disabled]="busy() || !valid()">Add task</button>
      </div>
    </form>

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (tasks().length === 0) {
      <div class="akd-empty">
        <p><strong>No scheduled tasks.</strong></p>
      </div>
    } @else {
      <table class="akd-table">
        <caption class="sr-only">Scheduled tasks of this application</caption>
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Command</th>
            <th scope="col">Schedule</th>
            <th scope="col">Next run</th>
            <th scope="col"><span class="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          @for (task of tasks(); track task.uuid) {
            <tr>
              <td>{{ task.name }}</td>
              <td class="akd-mono">{{ task.command }}</td>
              <td class="akd-mono">{{ task.cron_expression }}</td>
              <td class="akd-muted">{{ task.next_run_at ?? '—' }}</td>
              <td class="right">
                <button
                  class="akd-btn-ghost"
                  type="button"
                  [disabled]="busy()"
                  (click)="run(task)"
                >
                  Run now
                </button>
                <button
                  class="akd-btn-ghost"
                  type="button"
                  [disabled]="busy()"
                  (click)="startEdit(task)"
                >
                  Edit
                </button>
                <button
                  class="akd-btn-ghost"
                  type="button"
                  [attr.aria-expanded]="expanded() === task.uuid"
                  (click)="toggleHistory(task)"
                >
                  {{ expanded() === task.uuid ? 'Hide runs' : 'Runs' }}
                </button>
                <button
                  class="akd-btn-danger"
                  type="button"
                  [disabled]="busy()"
                  (click)="remove(task)"
                >
                  Delete
                </button>
              </td>
            </tr>
            @if (editing() === task.uuid) {
              <tr>
                <td colspan="5">
                  <form class="edit" (ngSubmit)="saveEdit(task)">
                    <div class="akd-field">
                      <label [for]="'te-name-' + task.uuid">Name</label>
                      <input
                        [id]="'te-name-' + task.uuid"
                        name="editName"
                        class="akd-input"
                        [(ngModel)]="editName"
                        [disabled]="busy()"
                      />
                    </div>
                    <div class="akd-field">
                      <label [for]="'te-command-' + task.uuid">Command</label>
                      <input
                        [id]="'te-command-' + task.uuid"
                        name="editCommand"
                        class="akd-input akd-mono"
                        [(ngModel)]="editCommand"
                        [disabled]="busy()"
                      />
                    </div>
                    <div class="akd-field">
                      <label [for]="'te-cron-' + task.uuid">Cron expression</label>
                      <input
                        [id]="'te-cron-' + task.uuid"
                        name="editCron"
                        class="akd-input akd-mono"
                        [(ngModel)]="editCron"
                        [disabled]="busy()"
                      />
                    </div>
                    <div class="edit-actions">
                      <button class="akd-btn" type="submit" [disabled]="busy()">Save</button>
                      <button class="akd-btn-ghost" type="button" (click)="editing.set(null)">
                        Cancel
                      </button>
                    </div>
                  </form>
                </td>
              </tr>
            }
            @if (expanded() === task.uuid) {
              <tr>
                <td colspan="5">
                  @if (executions().length === 0) {
                    <p class="akd-muted">Never run.</p>
                  } @else {
                    <table class="akd-table">
                      <caption class="sr-only">Recent runs of {{ task.name }}</caption>
                      <thead>
                        <tr>
                          <th scope="col">Status</th>
                          <th scope="col">Started</th>
                          <th scope="col">Exit code</th>
                          <th scope="col">Output</th>
                        </tr>
                      </thead>
                      <tbody>
                        @for (exec of executions(); track exec.uuid) {
                          <tr>
                            <td>
                              <akd-status-badge domain="task" [state]="exec.status" />
                              @if (exec.skip_reason) {
                                <span class="akd-muted"> {{ exec.skip_reason }}</span>
                              }
                            </td>
                            <td class="akd-muted">{{ exec.started_at }}</td>
                            <td class="akd-mono">{{ exec.exit_code ?? '—' }}</td>
                            <td>
                              @if (exec.output) {
                                <pre class="akd-mono output">{{ exec.output }}</pre>
                                @if (exec.output_truncated) {
                                  <p class="akd-muted">Output truncated by the server.</p>
                                }
                              } @else {
                                <span class="akd-muted">—</span>
                              }
                            </td>
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
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--akd-space-5);
        max-width: 32rem;
      }
      .edit {
        display: grid;
        gap: var(--akd-space-3);
        max-width: 32rem;
      }
      .edit-actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .output {
        margin: 0;
        max-height: 12rem;
        overflow: auto;
        white-space: pre-wrap;
        word-break: break-word;
      }
    `,
  ],
})
export class ApplicationTasksTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly tasks = signal<ScheduledTask[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly editing = signal<string | null>(null);
  protected readonly expanded = signal<string | null>(null);
  protected readonly executions = signal<TaskExecution[]>([]);

  protected name = '';
  protected command = '';
  protected cron = '';
  protected editName = '';
  protected editCommand = '';
  protected editCron = '';

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected valid(): boolean {
    return !!(this.name.trim() && this.command.trim() && this.cron.trim());
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const page = await this.api.client().listScheduledTasks(uuid, { limit: 100 });
      this.tasks.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createScheduledTask(this.uuid(), {
        name: this.name.trim(),
        command: this.command.trim(),
        cron_expression: this.cron.trim(),
        timezone: 'UTC',
        enabled: true,
        timeout_seconds: 300,
      });
      this.name = '';
      this.command = '';
      this.cron = '';
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected startEdit(task: ScheduledTask): void {
    this.editing.set(task.uuid);
    this.editName = task.name;
    this.editCommand = task.command;
    this.editCron = task.cron_expression;
  }

  protected async saveEdit(task: ScheduledTask): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      // If-Match carries the task's version: a concurrent edit gets a 409
      // instead of being silently overwritten (§24.1).
      await this.api.client().updateScheduledTask(task.uuid, task.version ?? 0, {
        name: this.editName.trim(),
        command: this.editCommand.trim(),
        cron_expression: this.editCron.trim(),
      });
      this.editing.set(null);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * Running by hand takes the same path as the cron — overlap policy included.
   * A 409 means the previous run is still going, which is exactly what the
   * operator needs to be told.
   */
  protected async run(task: ScheduledTask): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().runScheduledTask(task.uuid);
      if (this.expanded() === task.uuid) await this.loadExecutions(task.uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleHistory(task: ScheduledTask): Promise<void> {
    if (this.expanded() === task.uuid) {
      this.expanded.set(null);
      return;
    }
    this.expanded.set(task.uuid);
    this.executions.set([]);
    await this.loadExecutions(task.uuid);
  }

  private async loadExecutions(taskUuid: string): Promise<void> {
    try {
      const page = await this.api.client().listTaskExecutions(taskUuid, { limit: 10 });
      this.executions.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async remove(task: ScheduledTask): Promise<void> {
    if (
      !confirm(
        `Delete the task "${task.name}"? Its schedule stops and its run history is removed.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteScheduledTask(task.uuid);
      if (this.expanded() === task.uuid) this.expanded.set(null);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
