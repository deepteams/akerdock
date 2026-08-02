import type { components } from '../../api/schema';

type EnvVar = components['schemas']['EnvironmentVariable'];
type EnvVarUpdate = components['schemas']['EnvironmentVariableUpdate'];

/** The row being edited: what the inputs currently hold. */
export interface ResourceEnvEdit {
  value: string;
  literal: boolean;
}

/**
 * The value an edit form starts from. A redacted variable (no `read:sensitive`
 * or locked, INV-003) has no plaintext to show, so its field starts empty and
 * an empty field then means "keep the stored value".
 */
export function resourceEnvEditValue(env: EnvVar): string {
  return env.is_redacted ? '' : (env.value ?? '');
}

/**
 * The PATCH body for an edited variable, or `null` when nothing changed — a
 * no-op save must not bump `updated_at` nor write an audit entry.
 *
 * The key is identity: the API refuses to move it (recreate instead), so it is
 * never part of the payload. `is_multiline` follows the value it describes.
 */
export function resourceEnvUpdatePayload(env: EnvVar, edit: ResourceEnvEdit): EnvVarUpdate | null {
  const body: EnvVarUpdate = {};
  const initial = resourceEnvEditValue(env);
  // A redacted variable can be replaced but not blanked from here: an empty
  // field is the untouched state, not an instruction to erase.
  const valueChanged = env.is_redacted ? edit.value !== '' : edit.value !== initial;
  if (valueChanged) {
    body.value = edit.value;
    if (edit.value.includes('\n') !== env.is_multiline)
      body.is_multiline = edit.value.includes('\n');
  }
  if (edit.literal !== env.is_literal) body.is_literal = edit.literal;
  return Object.keys(body).length === 0 ? null : body;
}
