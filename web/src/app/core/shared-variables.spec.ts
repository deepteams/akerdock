import type { components } from '../../api/schema';
import { sharedVariableEditValue, sharedVariableUpdatePayload } from './shared-variables';

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
