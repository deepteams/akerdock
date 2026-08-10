import type { components } from '../../../api/schema';

type ScheduledTask = components['schemas']['ScheduledTask'];
type ScheduledTaskCreate = components['schemas']['ScheduledTaskCreate'];
type ScheduledTaskUpdate = components['schemas']['ScheduledTaskUpdate'];
type TaskKind = components['schemas']['TaskKind'];

/**
 * Form state is STRINGS, payloads are TYPES (same split as application-form).
 * These two builders are the single place where the task drawer and the inline
 * edit row become request bodies — which is what makes it testable that BOTH
 * of them carry a timezone. A cron without its zone is a cron in someone
 * else's afternoon (§24.3).
 */
export interface TaskForm {
  kind: TaskKind;
  name: string;
  /** `container_command` only. */
  command: string;
  /** `github_workflow` only. */
  workflowFile: string;
  /** `github_workflow` only; empty = the application's branch. */
  workflowRef: string;
  cron: string;
  /** IANA name the cron is evaluated in. */
  timezone: string;
}

/** A brand-new task: the operator's own zone, not a silent UTC. */
export function emptyTaskForm(timezone: string): TaskForm {
  return {
    kind: 'container_command',
    name: '',
    command: '',
    workflowFile: '',
    workflowRef: '',
    cron: '',
    timezone,
  };
}

/**
 * An edit opens on what is stored, never on the browser's zone: re-saving a
 * task must not move it to the editor's timezone. `UTC` only stands in when
 * the API sent no zone at all — which is its own default.
 */
export function taskFormFromTask(task: ScheduledTask): TaskForm {
  return {
    kind: task.kind,
    name: task.name,
    command: task.command ?? '',
    workflowFile: task.workflow_file ?? '',
    workflowRef: task.workflow_ref ?? '',
    cron: task.cron_expression,
    timezone: task.timezone || 'UTC',
  };
}

export function taskFormValid(form: TaskForm): boolean {
  if (!form.name.trim() || !form.cron.trim() || !form.timezone.trim()) return false;
  if (form.kind === 'github_workflow') return !!form.workflowFile.trim();
  return !!form.command.trim();
}

/**
 * The two kinds have disjoint fields (ADR-071): the API refuses a command on a
 * workflow task, so the body only carries the chosen side.
 */
export function taskCreateBody(form: TaskForm): ScheduledTaskCreate {
  const base = {
    kind: form.kind,
    name: form.name.trim(),
    cron_expression: form.cron.trim(),
    timezone: form.timezone.trim(),
    enabled: true,
    timeout_seconds: 300,
  };
  return form.kind === 'github_workflow'
    ? {
        ...base,
        workflow_file: form.workflowFile.trim(),
        workflow_ref: form.workflowRef.trim() || null,
      }
    : { ...base, command: form.command.trim() };
}

/** Same disjunction on the patch side; `kind` itself is immutable. */
export function taskUpdateBody(form: TaskForm): ScheduledTaskUpdate {
  const base = {
    name: form.name.trim(),
    cron_expression: form.cron.trim(),
    timezone: form.timezone.trim(),
  };
  return form.kind === 'github_workflow'
    ? {
        ...base,
        workflow_file: form.workflowFile.trim(),
        workflow_ref: form.workflowRef.trim() || null,
      }
    : { ...base, command: form.command.trim() };
}
