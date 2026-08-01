import type { components } from '../../api/schema';

type SharedVariable = components['schemas']['SharedVariable'];
type SharedVariableUpdate = components['schemas']['SharedVariableUpdate'];

/** The row being edited: what the inputs currently hold. */
export interface SharedVariableEdit {
  value: string;
  secret: boolean;
}

/**
 * The value an edit form starts from. A redacted variable (no `read:sensitive`,
 * INV-003) has no plaintext to show, so its field starts empty and an empty
 * field then means "keep the stored value" — the only honest reading when the
 * form never held it.
 */
export function sharedVariableEditValue(variable: SharedVariable): string {
  return variable.is_redacted ? '' : (variable.value ?? '');
}

/**
 * The PATCH body for an edited variable, or `null` when nothing changed — a
 * no-op save must not bump `updated_at` nor write an audit entry.
 *
 * The key and the scope are identity: the API refuses to move them (recreate
 * instead), so they are never part of the payload.
 */
export function sharedVariableUpdatePayload(
  variable: SharedVariable,
  edit: SharedVariableEdit,
): SharedVariableUpdate | null {
  const body: SharedVariableUpdate = {};
  const initial = sharedVariableEditValue(variable);
  // A redacted variable can be replaced but not blanked from here: an empty
  // field is the untouched state, not an instruction to erase.
  const valueChanged = variable.is_redacted ? edit.value !== '' : edit.value !== initial;
  if (valueChanged) body.value = edit.value;
  if (edit.secret !== variable.is_secret) body.is_secret = edit.secret;
  return body.value === undefined && body.is_secret === undefined ? null : body;
}
