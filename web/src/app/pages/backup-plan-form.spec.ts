import type { components } from '../../api/schema';
import {
  backupPlanCreateBody,
  backupPlanEditForm,
  backupPlanUpdateBody,
  emptyBackupPlanForm,
} from './backup-plan-form';

type BackupPlan = components['schemas']['BackupPlan'];

function aPlan(overrides: Partial<BackupPlan> = {}): BackupPlan {
  return {
    uuid: 'plan-1',
    frequency: 'daily',
    timezone: 'Europe/Paris',
    enabled: true,
    version: 2,
    ...overrides,
  } as BackupPlan;
}

describe('backup plan create body', () => {
  it('starts a new plan in the zone it is given', () => {
    expect(emptyBackupPlanForm('Europe/Paris').timezone).toBe('Europe/Paris');
    expect(emptyBackupPlanForm('Europe/Paris').frequency).toBe('daily');
  });

  it('sends the chosen timezone, not a hardcoded UTC', () => {
    const body = backupPlanCreateBody({
      frequency: ' 0 3 * * * ',
      timezone: ' Asia/Tokyo ',
      retentionCount: 7,
      retentionDays: 30,
      s3Uuid: '',
    });

    expect(body.timezone).toBe('Asia/Tokyo');
    expect(body.frequency).toBe('0 3 * * *');
    expect(body.save_s3).toBeFalse();
    expect(body.s3_storage_uuid).toBeNull();
    expect(body.local_retention).toEqual({ max_count: 7, max_age_days: 30, max_size_gb: 0 });
  });

  it('turns an S3 target into save_s3', () => {
    const body = backupPlanCreateBody({
      ...emptyBackupPlanForm('UTC'),
      s3Uuid: 's3-1',
    });

    expect(body.save_s3).toBeTrue();
    expect(body.s3_storage_uuid).toBe('s3-1');
  });
});

describe('backup plan update body', () => {
  it('opens an edit on the stored zone', () => {
    expect(backupPlanEditForm(aPlan({ timezone: 'America/New_York' })).timezone).toBe(
      'America/New_York',
    );
  });

  it('falls back to UTC only when the plan carries no zone', () => {
    expect(backupPlanEditForm(aPlan({ timezone: undefined })).timezone).toBe('UTC');
  });

  // The edit row used to send only frequency and enabled: a plan could never
  // be moved out of the zone it was created in.
  it('sends the timezone on edit', () => {
    const body = backupPlanUpdateBody({
      frequency: ' hourly ',
      timezone: ' Europe/Paris ',
      enabled: false,
    });

    expect(body).toEqual({ frequency: 'hourly', timezone: 'Europe/Paris', enabled: false });
  });

  it('round-trips a stored zone untouched', () => {
    const plan = aPlan({ timezone: 'Australia/Sydney' });

    expect(backupPlanUpdateBody(backupPlanEditForm(plan)).timezone).toBe('Australia/Sydney');
  });
});
