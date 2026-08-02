import type { components } from '../../api/schema';

type SharedVariable = components['schemas']['SharedVariable'];
type SharedVariableCreate = components['schemas']['SharedVariableCreate'];
type SharedVariableUpdate = components['schemas']['SharedVariableUpdate'];

/** The inheritance levels of §5.4 — `team` has no parent, the others do. */
export type SharedVariableScope = SharedVariable['scope'];

/** A scope that hangs off one parent entity: the levels a detail page owns. */
export type ScopedSharedVariableScope = Exclude<SharedVariableScope, 'team'>;

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

/** The UUID of the entity a variable hangs off, or null for the team scope. */
export function sharedVariableParentUuid(variable: SharedVariable): string | null {
  switch (variable.scope) {
    case 'project':
      return variable.project_uuid ?? null;
    case 'environment':
      return variable.environment_uuid ?? null;
    case 'server':
      return variable.server_uuid ?? null;
    default:
      return null;
  }
}

/**
 * The variables of ONE parent, sorted by key. `GET /shared-variables` filters
 * by scope alone, so the narrowing to a single project/environment/server is
 * ours to do.
 */
export function sharedVariablesOf(
  variables: readonly SharedVariable[],
  scope: ScopedSharedVariableScope,
  parentUuid: string,
): SharedVariable[] {
  return variables
    .filter((v) => v.scope === scope && sharedVariableParentUuid(v) === parentUuid)
    .sort((a, b) => a.key.localeCompare(b.key));
}

/** The POST body for a new variable — the parent UUID lands in the field its scope names. */
export function sharedVariableCreatePayload(
  scope: ScopedSharedVariableScope,
  parentUuid: string,
  draft: { key: string; value: string; secret: boolean },
): SharedVariableCreate {
  return {
    scope,
    [`${scope}_uuid`]: parentUuid,
    key: draft.key.trim(),
    value: draft.value,
    is_secret: draft.secret,
  };
}
