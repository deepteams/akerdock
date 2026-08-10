import type { components } from '../../api/schema';

type BackupPlan = components['schemas']['BackupPlan'];
type BackupPlanCreate = components['schemas']['BackupPlanCreate'];
type BackupPlanUpdate = components['schemas']['BackupPlanUpdate'];

/**
 * The backup plan form of the database page, turned into request bodies.
 * Extracted for the same reason as the task form: the create row and the
 * inline edit row must both carry a timezone, and that is a claim worth a
 * test rather than a reading (§7.1, §24.3).
 */
export interface BackupPlanForm {
  /** Cron expression or alias (`daily`, `hourly`…). */
  frequency: string;
  /** IANA name the frequency is evaluated in. */
  timezone: string;
  retentionCount: number;
  retentionDays: number;
  /** Empty = local only. */
  s3Uuid: string;
}

/** A brand-new plan: the operator's own zone, not a silent UTC. */
export function emptyBackupPlanForm(timezone: string): BackupPlanForm {
  return { frequency: 'daily', timezone, retentionCount: 7, retentionDays: 0, s3Uuid: '' };
}

export function backupPlanCreateBody(form: BackupPlanForm): BackupPlanCreate {
  return {
    frequency: form.frequency.trim(),
    timezone: form.timezone.trim(),
    enabled: true,
    dump_all: false,
    databases_to_include: [],
    save_local: true,
    save_s3: !!form.s3Uuid,
    s3_storage_uuid: form.s3Uuid || null,
    s3_only: false,
    local_retention: {
      max_count: form.retentionCount,
      max_age_days: form.retentionDays,
      max_size_gb: 0,
    },
    timeout_seconds: 3600,
    drill_enabled: false,
    drill_interval_days: 7,
  };
}

/** What the inline edit row can change. */
export interface BackupPlanEditForm {
  frequency: string;
  timezone: string;
  enabled: boolean;
}

/** An edit opens on the stored zone — re-saving never moves the plan. */
export function backupPlanEditForm(plan: BackupPlan): BackupPlanEditForm {
  return {
    frequency: plan.frequency,
    timezone: plan.timezone || 'UTC',
    enabled: plan.enabled,
  };
}

export function backupPlanUpdateBody(form: BackupPlanEditForm): BackupPlanUpdate {
  return {
    frequency: form.frequency.trim(),
    timezone: form.timezone.trim(),
    enabled: form.enabled,
  };
}
