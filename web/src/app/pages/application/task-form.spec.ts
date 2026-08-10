import type { components } from '../../../api/schema';
import {
  emptyTaskForm,
  taskCreateBody,
  taskFormFromTask,
  taskFormValid,
  taskUpdateBody,
  type TaskForm,
} from './task-form';

type ScheduledTask = components['schemas']['ScheduledTask'];

function aTask(overrides: Partial<ScheduledTask> = {}): ScheduledTask {
  return {
    uuid: 'task-1',
    kind: 'container_command',
    name: 'nightly',
    command: 'php artisan schedule:run',
    cron_expression: '0 3 * * *',
    timezone: 'Europe/Paris',
    version: 4,
    ...overrides,
  } as ScheduledTask;
}

function aForm(overrides: Partial<TaskForm> = {}): TaskForm {
  return { ...emptyTaskForm('Europe/Paris'), name: 'nightly', cron: '0 3 * * *', ...overrides };
}

describe('task form defaults', () => {
  it('starts a new task in the zone it is given', () => {
    expect(emptyTaskForm('Europe/Paris').timezone).toBe('Europe/Paris');
    expect(emptyTaskForm('UTC').timezone).toBe('UTC');
  });

  it('opens an edit on the stored zone, never on the editor own zone', () => {
    expect(taskFormFromTask(aTask({ timezone: 'Asia/Tokyo' })).timezone).toBe('Asia/Tokyo');
  });

  it('falls back to UTC only when the task carries no zone', () => {
    expect(taskFormFromTask(aTask({ timezone: undefined })).timezone).toBe('UTC');
  });

  it('reads the rest of the task into the form', () => {
    const form = taskFormFromTask(
      aTask({
        kind: 'github_workflow',
        command: null,
        workflow_file: 'build.yml',
        workflow_ref: 'main',
      }),
    );

    expect(form).toEqual({
      kind: 'github_workflow',
      name: 'nightly',
      command: '',
      workflowFile: 'build.yml',
      workflowRef: 'main',
      cron: '0 3 * * *',
      timezone: 'Europe/Paris',
    });
  });

  it('refuses a form without a name, a cron or a zone', () => {
    expect(taskFormValid(aForm({ command: 'ls' }))).toBeTrue();
    expect(taskFormValid(aForm({ command: 'ls', name: '  ' }))).toBeFalse();
    expect(taskFormValid(aForm({ command: 'ls', cron: '' }))).toBeFalse();
    expect(taskFormValid(aForm({ command: 'ls', timezone: '' }))).toBeFalse();
    expect(taskFormValid(aForm({ command: '' }))).toBeFalse();
    expect(taskFormValid(aForm({ kind: 'github_workflow', workflowFile: 'b.yml' }))).toBeTrue();
    expect(taskFormValid(aForm({ kind: 'github_workflow' }))).toBeFalse();
  });
});

describe('task create body', () => {
  it('sends the chosen timezone, not a hardcoded UTC', () => {
    const body = taskCreateBody(aForm({ command: 'ls', timezone: 'Europe/Paris' }));

    expect(body.timezone).toBe('Europe/Paris');
    expect(body).toEqual({
      kind: 'container_command',
      name: 'nightly',
      cron_expression: '0 3 * * *',
      timezone: 'Europe/Paris',
      enabled: true,
      timeout_seconds: 300,
      command: 'ls',
    });
  });

  it('sends the timezone on a workflow task too, with no command', () => {
    const body = taskCreateBody(
      aForm({ kind: 'github_workflow', workflowFile: 'build.yml', timezone: 'Asia/Tokyo' }),
    );

    expect(body.timezone).toBe('Asia/Tokyo');
    expect(body.workflow_file).toBe('build.yml');
    expect(body.workflow_ref).toBeNull();
    expect('command' in body).toBeFalse();
  });

  it('trims what the operator typed', () => {
    const body = taskCreateBody(
      aForm({
        name: '  nightly ',
        cron: ' 0 3 * * * ',
        timezone: ' Europe/Paris ',
        command: ' ls ',
      }),
    );

    expect(body).toEqual(
      jasmine.objectContaining({
        name: 'nightly',
        cron_expression: '0 3 * * *',
        timezone: 'Europe/Paris',
        command: 'ls',
      }),
    );
  });
});

describe('task update body', () => {
  // The edit form used to send no timezone at all: a task saved from Paris
  // silently kept whatever zone the create form had guessed.
  it('sends the timezone on edit', () => {
    const body = taskUpdateBody(aForm({ command: 'ls', timezone: 'Europe/Paris' }));

    expect(body.timezone).toBe('Europe/Paris');
    expect(body).toEqual({
      name: 'nightly',
      cron_expression: '0 3 * * *',
      timezone: 'Europe/Paris',
      command: 'ls',
    });
  });

  it('keeps the two kinds disjoint on the patch as well', () => {
    const body = taskUpdateBody(
      aForm({ kind: 'github_workflow', workflowFile: 'build.yml', workflowRef: '  ' }),
    );

    expect(body).toEqual({
      name: 'nightly',
      cron_expression: '0 3 * * *',
      timezone: 'Europe/Paris',
      workflow_file: 'build.yml',
      workflow_ref: null,
    });
  });

  it('round-trips a stored zone untouched', () => {
    const task = aTask({ timezone: 'America/Sao_Paulo' });

    expect(taskUpdateBody(taskFormFromTask(task)).timezone).toBe('America/Sao_Paulo');
  });
});
