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
import { CardComponent } from '../../../ui/card/card.component';
import { DrawerComponent } from '../../../ui/drawer/drawer.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { fetchAll } from '../../core/pagination';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import type { components } from '../../../api/schema';

type ScheduledTask = components['schemas']['ScheduledTask'];
type TaskExecution = components['schemas']['TaskExecution'];

@Component({
  selector: 'app-application-tasks-tab',
  standalone: true,
  imports: [
    FormsModule,
    StatusBadgeComponent,
    CardComponent,
    DrawerComponent,
    EmptyStateComponent,
    IconComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <div class="bar">
      <button class="akd-btn akd-btn--primary" type="button" (click)="openAdd()" [disabled]="busy()">
        <akd-icon name="plus" [size]="15" />
        Add task
      </button>
    </div>

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (tasks().length === 0) {
      <akd-empty-state icon="clock" title="No scheduled tasks" />
    } @else {
      <akd-card title="Scheduled tasks" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Scheduled tasks of this application
          </caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Action</th>
              <th scope="col">Schedule</th>
              <th scope="col">Next run</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (task of tasks(); track task.uuid) {
              <tr>
                <td>{{ task.name }}</td>
                <td class="akd-mono">
                  @if (task.kind === 'github_workflow') {
                    <span class="akd-badge">workflow</span>
                    {{ task.workflow_file }}
                    @if (task.workflow_ref) {
                      <span class="akd-muted">&#64; {{ task.workflow_ref }}</span>
                    }
                  } @else {
                    {{ task.command }}
                  }
                </td>
                <td>
                  <span class="akd-badge akd-badge--mono">{{ task.cron_expression }}</span>
                </td>
                <td class="akd-muted">{{ task.next_run_at ?? '—' }}</td>
                <td class="right">
                  <div class="row-actions">
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="run(task)"
                    >
                      <akd-icon name="play" [size]="13" />
                      Run now
                    </button>
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="startEdit(task)"
                    >
                      <akd-icon name="pencil" [size]="13" />
                      Edit
                    </button>
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [attr.aria-expanded]="expanded() === task.uuid"
                      (click)="toggleHistory(task)"
                    >
                      {{ expanded() === task.uuid ? 'Hide runs' : 'Runs' }}
                    </button>
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(task)"
                    >
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
              @if (editing() === task.uuid) {
                <tr>
                  <td colspan="5">
                    <form class="edit" (ngSubmit)="saveEdit(task)">
                      <div class="akd-field">
                        <label class="akd-field__label" [for]="'te-name-' + task.uuid">Name</label>
                        <input
                          [id]="'te-name-' + task.uuid"
                          name="editName"
                          class="akd-input"
                          [(ngModel)]="editName"
                          [disabled]="busy()"
                        />
                      </div>
                      @if (task.kind === 'github_workflow') {
                        <div class="akd-field">
                          <label class="akd-field__label" [for]="'te-wf-' + task.uuid">
                            Workflow file
                          </label>
                          <input
                            [id]="'te-wf-' + task.uuid"
                            name="editWorkflowFile"
                            class="akd-input akd-input--mono"
                            [(ngModel)]="editWorkflowFile"
                            [disabled]="busy()"
                          />
                        </div>
                        <div class="akd-field">
                          <label class="akd-field__label" [for]="'te-ref-' + task.uuid">
                            Git ref (empty = the application's branch)
                          </label>
                          <input
                            [id]="'te-ref-' + task.uuid"
                            name="editWorkflowRef"
                            class="akd-input akd-input--mono"
                            [(ngModel)]="editWorkflowRef"
                            [disabled]="busy()"
                          />
                        </div>
                      } @else {
                        <div class="akd-field">
                          <label class="akd-field__label" [for]="'te-command-' + task.uuid">
                            Command
                          </label>
                          <input
                            [id]="'te-command-' + task.uuid"
                            name="editCommand"
                            class="akd-input akd-input--mono"
                            [(ngModel)]="editCommand"
                            [disabled]="busy()"
                          />
                        </div>
                      }
                      <div class="akd-field">
                        <label class="akd-field__label" [for]="'te-cron-' + task.uuid">
                          Cron expression
                        </label>
                        <input
                          [id]="'te-cron-' + task.uuid"
                          name="editCron"
                          class="akd-input akd-input--mono"
                          [(ngModel)]="editCron"
                          [disabled]="busy()"
                        />
                      </div>
                      <div class="edit-actions">
                        <button
                          class="akd-btn akd-btn--primary akd-btn--sm"
                          type="submit"
                          [disabled]="busy()"
                        >
                          Save
                        </button>
                        <button
                          class="akd-btn akd-btn--secondary akd-btn--sm"
                          type="button"
                          (click)="editing.set(null)"
                        >
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
                        <caption class="sr-only">
                          Recent runs of
                          {{
                            task.name
                          }}
                        </caption>
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
      </akd-card>
    }

    <akd-drawer [open]="showAdd()" title="Add scheduled task" (closed)="closeAdd()">
      <form id="task-form" class="form" (ngSubmit)="create()">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <div class="akd-field">
          <label class="akd-field__label" for="tk-name">Name</label>
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
          <label class="akd-field__label" for="tk-kind">What the task does</label>
          <select id="tk-kind" name="kind" class="akd-input" [(ngModel)]="kind" [disabled]="busy()">
            <option value="container_command">Run a command in the container</option>
            <option value="github_workflow">Dispatch a GitHub Actions workflow</option>
          </select>
          @if (kind === 'github_workflow') {
            <span class="akd-field__hint">
              Fired by the application's GitHub App — a reliable replacement for the workflow's
              own <code>on: schedule</code>. The App needs the <code>actions: write</code>
              permission.
            </span>
          }
        </div>
        @if (kind === 'github_workflow') {
          <div class="akd-field">
            <label class="akd-field__label" for="tk-workflow">Workflow file</label>
            <input
              id="tk-workflow"
              name="workflowFile"
              class="akd-input akd-input--mono"
              placeholder="build.yml"
              required
              [(ngModel)]="workflowFile"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="tk-ref">Git ref (optional)</label>
            <input
              id="tk-ref"
              name="workflowRef"
              class="akd-input akd-input--mono"
              placeholder="main"
              [(ngModel)]="workflowRef"
              [disabled]="busy()"
            />
            <span class="akd-field__hint">
              Empty = the application's branch, then the repository's default branch.
            </span>
          </div>
        } @else {
          <div class="akd-field">
            <label class="akd-field__label" for="tk-command">Command (run in the container)</label>
            <input
              id="tk-command"
              name="command"
              class="akd-input akd-input--mono"
              placeholder="php artisan schedule:run"
              required
              [(ngModel)]="command"
              [disabled]="busy()"
            />
          </div>
        }
        <div class="akd-field">
          <label class="akd-field__label" for="tk-cron">Cron expression</label>
          <input
            id="tk-cron"
            name="cron"
            class="akd-input akd-input--mono"
            placeholder="0 3 * * *"
            required
            [(ngModel)]="cron"
            [disabled]="busy()"
          />
          <span class="akd-field__hint">Standard 5-field cron, evaluated in UTC.</span>
        </div>
      </form>
      <div drawer-footer>
        <button class="akd-btn akd-btn--ghost" type="button" (click)="closeAdd()" [disabled]="busy()">
          Cancel
        </button>
        <button
          class="akd-btn akd-btn--primary"
          type="submit"
          form="task-form"
          [disabled]="busy() || !valid()"
        >
          <akd-icon name="plus" [size]="15" />
          Add task
        </button>
      </div>
    </akd-drawer>
  `,
  styles: [
    `
      .bar {
        display: flex;
        justify-content: flex-end;
        margin-bottom: var(--space-5);
      }
      .form {
        display: grid;
        gap: var(--space-3);
      }
      .edit {
        display: grid;
        gap: var(--space-3);
        max-width: 32rem;
        padding: var(--space-3) 0;
      }
      .edit-actions,
      .row-actions {
        display: flex;
        gap: var(--space-2);
      }
      .row-actions {
        justify-content: flex-end;
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
  private readonly confirm = inject(ConfirmService);

  protected readonly tasks = signal<ScheduledTask[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly editing = signal<string | null>(null);
  protected readonly expanded = signal<string | null>(null);
  protected readonly executions = signal<TaskExecution[]>([]);
  protected readonly showAdd = signal(false);

  protected name = '';
  protected kind: components['schemas']['TaskKind'] = 'container_command';
  protected command = '';
  protected workflowFile = '';
  protected workflowRef = '';
  protected cron = '';

  protected openAdd(): void {
    this.name = '';
    this.kind = 'container_command';
    this.command = '';
    this.workflowFile = '';
    this.workflowRef = '';
    this.cron = '';
    this.error.set(null);
    this.showAdd.set(true);
  }

  protected closeAdd(): void {
    if (this.busy()) return;
    this.showAdd.set(false);
  }
  protected editName = '';
  protected editCommand = '';
  protected editWorkflowFile = '';
  protected editWorkflowRef = '';
  protected editCron = '';

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected valid(): boolean {
    if (!this.name.trim() || !this.cron.trim()) return false;
    if (this.kind === 'github_workflow') return !!this.workflowFile.trim();
    return !!this.command.trim();
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const tasks = await fetchAll((cursor) =>
        this.api.client().listScheduledTasks(uuid, { limit: 100, cursor }),
      );
      this.tasks.set(tasks);
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
      // The two kinds have disjoint fields (ADR-071): the API refuses a
      // command on a workflow task, so the body only carries the chosen side.
      const base = {
        kind: this.kind,
        name: this.name.trim(),
        cron_expression: this.cron.trim(),
        timezone: 'UTC',
        enabled: true,
        timeout_seconds: 300,
      };
      await this.api.client().createScheduledTask(
        this.uuid(),
        this.kind === 'github_workflow'
          ? {
              ...base,
              workflow_file: this.workflowFile.trim(),
              workflow_ref: this.workflowRef.trim() || null,
            }
          : { ...base, command: this.command.trim() },
      );
      this.name = '';
      this.command = '';
      this.workflowFile = '';
      this.workflowRef = '';
      this.cron = '';
      this.showAdd.set(false);
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
    this.editCommand = task.command ?? '';
    this.editWorkflowFile = task.workflow_file ?? '';
    this.editWorkflowRef = task.workflow_ref ?? '';
    this.editCron = task.cron_expression;
  }

  protected async saveEdit(task: ScheduledTask): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      // If-Match carries the task's version: a concurrent edit gets a 409
      // instead of being silently overwritten (§24.1). The patch only carries
      // the fields of the task's own kind — the API refuses the others.
      await this.api.client().updateScheduledTask(
        task.uuid,
        task.version ?? 0,
        task.kind === 'github_workflow'
          ? {
              name: this.editName.trim(),
              workflow_file: this.editWorkflowFile.trim(),
              workflow_ref: this.editWorkflowRef.trim() || null,
              cron_expression: this.editCron.trim(),
            }
          : {
              name: this.editName.trim(),
              command: this.editCommand.trim(),
              cron_expression: this.editCron.trim(),
            },
      );
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
      !(await this.confirm.ask({
        title: 'Delete the task',
        message: `Delete the task "${task.name}"? Its schedule stops and its run history is removed.`,
        confirmLabel: 'Delete',
      }))
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
