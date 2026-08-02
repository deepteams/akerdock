import type { components } from '../../api/schema';
import {
  sharedVariableCreatePayload,
  sharedVariableEditValue,
  sharedVariableParentUuid,
  sharedVariableUpdatePayload,
  sharedVariablesOf,
} from './shared-variables';

type SharedVariable = components['schemas']['SharedVariable'];

function variable(patch: Partial<SharedVariable> = {}): SharedVariable {
  return {
    uuid: 'v1',
    scope: 'environment',
    key: 'DB_DSN',
    value: 'postgres://x',
    is_redacted: false,
    is_secret: false,
    created_at: '2026-01-01T00:00:00Z',
    ...patch,
  } as SharedVariable;
}

describe('sharedVariableEditValue', () => {
  it('seeds the field with the plaintext when it is readable', () => {
    expect(sharedVariableEditValue(variable())).toBe('postgres://x');
  });

  it('starts empty for a redacted variable — the form never held the value', () => {
    expect(sharedVariableEditValue(variable({ is_redacted: true, value: null }))).toBe('');
  });
});

describe('sharedVariableUpdatePayload', () => {
  it('returns null when nothing changed — no PATCH, no audit entry', () => {
    expect(sharedVariableUpdatePayload(variable(), { value: 'postgres://x', secret: false })).toBe(
      null,
    );
  });

  it('sends only the value when the value alone changed', () => {
    expect(
      sharedVariableUpdatePayload(variable(), { value: 'postgres://y', secret: false }),
    ).toEqual({ value: 'postgres://y' });
  });

  it('sends only the flag when the masking alone changed', () => {
    expect(
      sharedVariableUpdatePayload(variable(), { value: 'postgres://x', secret: true }),
    ).toEqual({
      is_secret: true,
    });
  });

  it('sends both when both changed', () => {
    expect(sharedVariableUpdatePayload(variable(), { value: 'z', secret: true })).toEqual({
      value: 'z',
      is_secret: true,
    });
  });

  it('blanks a readable value on request', () => {
    expect(sharedVariableUpdatePayload(variable(), { value: '', secret: false })).toEqual({
      value: '',
    });
  });

  it('keeps a redacted value when the field is left empty', () => {
    const redacted = variable({ is_redacted: true, value: null, is_secret: true });
    expect(sharedVariableUpdatePayload(redacted, { value: '', secret: true })).toBe(null);
    // …and the masking can still be lifted without touching the value.
    expect(sharedVariableUpdatePayload(redacted, { value: '', secret: false })).toEqual({
      is_secret: false,
    });
  });

  it('replaces a redacted value when the field is filled', () => {
    const redacted = variable({ is_redacted: true, value: null, is_secret: true });
    expect(sharedVariableUpdatePayload(redacted, { value: 'new', secret: true })).toEqual({
      value: 'new',
    });
  });
});

describe('sharedVariableParentUuid', () => {
  it('reads the parent named by the scope', () => {
    expect(sharedVariableParentUuid(variable({ scope: 'project', project_uuid: 'p1' }))).toBe('p1');
    expect(sharedVariableParentUuid(variable({ environment_uuid: 'e1' }))).toBe('e1');
    expect(sharedVariableParentUuid(variable({ scope: 'server', server_uuid: 's1' }))).toBe('s1');
  });

  it('has none for the team scope', () => {
    expect(sharedVariableParentUuid(variable({ scope: 'team' }))).toBe(null);
  });

  it('ignores a sibling parent left on the row', () => {
    // A server-scoped row carrying a stale project_uuid must not answer with it.
    expect(sharedVariableParentUuid(variable({ scope: 'server', project_uuid: 'p1' }))).toBe(null);
  });
});

describe('sharedVariablesOf', () => {
  const list = [
    variable({ uuid: 'b', scope: 'server', server_uuid: 's1', key: 'B' }),
    variable({ uuid: 'a', scope: 'server', server_uuid: 's1', key: 'A' }),
    variable({ uuid: 'other', scope: 'server', server_uuid: 's2', key: 'A' }),
    variable({ uuid: 'env', scope: 'environment', environment_uuid: 's1', key: 'A' }),
  ];

  it('keeps only the variables of that one parent, sorted by key', () => {
    expect(sharedVariablesOf(list, 'server', 's1').map((v) => v.uuid)).toEqual(['a', 'b']);
  });

  it('does not confuse two parents that share a UUID across scopes', () => {
    expect(sharedVariablesOf(list, 'environment', 's1').map((v) => v.uuid)).toEqual(['env']);
  });

  it('is empty for a parent with nothing of its own', () => {
    expect(sharedVariablesOf(list, 'project', 'p1')).toEqual([]);
  });
});

describe('sharedVariableCreatePayload', () => {
  it('puts the parent UUID in the field its scope names', () => {
    expect(
      sharedVariableCreatePayload('project', 'p1', { key: 'API_URL', value: 'x', secret: false }),
    ).toEqual({
      scope: 'project',
      project_uuid: 'p1',
      key: 'API_URL',
      value: 'x',
      is_secret: false,
    });
    expect(
      sharedVariableCreatePayload('server', 's1', { key: 'K', value: 'v', secret: true }),
    ).toEqual({ scope: 'server', server_uuid: 's1', key: 'K', value: 'v', is_secret: true });
  });

  it('trims the key but never the value — trailing spaces can be meaningful', () => {
    expect(
      sharedVariableCreatePayload('environment', 'e1', {
        key: '  K  ',
        value: ' v ',
        secret: false,
      }),
    ).toEqual({
      scope: 'environment',
      environment_uuid: 'e1',
      key: 'K',
      value: ' v ',
      is_secret: false,
    });
  });
});
