import type { components } from '../../api/schema';
import { resourceEnvEditValue, resourceEnvUpdatePayload } from './resource-envs';

type EnvVar = components['schemas']['EnvironmentVariable'];

function env(patch: Partial<EnvVar> = {}): EnvVar {
  return {
    uuid: 'e1',
    key: 'API_URL',
    value: 'https://api',
    is_redacted: false,
    is_secret: false,
    is_build_time: false,
    is_literal: false,
    is_multiline: false,
    is_locked: false,
    created_at: '2026-01-01T00:00:00Z',
    ...patch,
  } as EnvVar;
}

describe('resourceEnvEditValue', () => {
  it('seeds the field with the plaintext when it is readable', () => {
    expect(resourceEnvEditValue(env())).toBe('https://api');
  });

  it('starts empty for a redacted variable — the form never held the value', () => {
    expect(resourceEnvEditValue(env({ is_redacted: true, value: null }))).toBe('');
  });
});

describe('resourceEnvUpdatePayload', () => {
  it('returns null when nothing changed — no PATCH, no audit entry', () => {
    expect(resourceEnvUpdatePayload(env(), { value: 'https://api', literal: false })).toBe(null);
  });

  it('sends only the value when the value alone changed', () => {
    expect(resourceEnvUpdatePayload(env(), { value: 'https://other', literal: false })).toEqual({
      value: 'https://other',
    });
  });

  it('sends only the flag when the literal flag alone changed', () => {
    expect(resourceEnvUpdatePayload(env(), { value: 'https://api', literal: true })).toEqual({
      is_literal: true,
    });
  });

  it('follows the value with is_multiline when newlines appear', () => {
    expect(resourceEnvUpdatePayload(env(), { value: 'a\nb', literal: false })).toEqual({
      value: 'a\nb',
      is_multiline: true,
    });
  });

  it('clears is_multiline when the newlines are gone', () => {
    const multi = env({ value: 'a\nb', is_multiline: true });
    expect(resourceEnvUpdatePayload(multi, { value: 'a', literal: false })).toEqual({
      value: 'a',
      is_multiline: false,
    });
  });

  it('keeps a redacted value when the field is left empty', () => {
    const redacted = env({ is_redacted: true, value: null, is_literal: true });
    expect(resourceEnvUpdatePayload(redacted, { value: '', literal: true })).toBe(null);
    // …and the literal flag can still be flipped without touching the value.
    expect(resourceEnvUpdatePayload(redacted, { value: '', literal: false })).toEqual({
      is_literal: false,
    });
  });

  it('replaces a redacted value when the field is filled', () => {
    const redacted = env({ is_redacted: true, value: null });
    expect(resourceEnvUpdatePayload(redacted, { value: 'new', literal: false })).toEqual({
      value: 'new',
    });
  });

  it('blanks a readable value on request', () => {
    expect(resourceEnvUpdatePayload(env(), { value: '', literal: false })).toEqual({ value: '' });
  });
});
