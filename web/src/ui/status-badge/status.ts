// The state → family mapping of design-system §2.3. It is the truth table of
// akd-status-badge, and it is EXHAUSTIVE: every state of the §21 machines maps
// to exactly one family.
//
// Principle 4 of the design system: "one state = one representation everywhere".
// A state must look the same on the dashboard, in a table row, on a resource
// card and in a deployment timeline — which is only true if a single component
// owns this table. Re-styling a state locally is forbidden.

export type StatusFamily = 'success' | 'progress' | 'warning' | 'danger' | 'neutral';

/** Shape modifiers (§2.3): never colour alone (WCAG 1.4.1). */
export type StatusModifier = 'none' | 'stale' | 'superseded';

export interface StatusMeaning {
  family: StatusFamily;
  modifier: StatusModifier;
}

/**
 * Deployment states (§21.1). `queued` through `finishing` are transitional and
 * animate; `cancelled` and `superseded` are terminal-but-abandoned, and get a
 * struck-through label so they are distinguishable without colour perception.
 */
const DEPLOYMENT: Record<string, StatusMeaning> = {
  queued: { family: 'progress', modifier: 'none' },
  preparing: { family: 'progress', modifier: 'none' },
  cloning: { family: 'progress', modifier: 'none' },
  building: { family: 'progress', modifier: 'none' },
  pushing: { family: 'progress', modifier: 'none' },
  starting: { family: 'progress', modifier: 'none' },
  healthchecking: { family: 'progress', modifier: 'none' },
  switching: { family: 'progress', modifier: 'none' },
  finishing: { family: 'progress', modifier: 'none' },
  retrying: { family: 'progress', modifier: 'none' },
  succeeded: { family: 'success', modifier: 'none' },
  failed: { family: 'danger', modifier: 'none' },
  cancelled: { family: 'neutral', modifier: 'superseded' },
  superseded: { family: 'neutral', modifier: 'superseded' },
};

/**
 * Resource and server states (§21.2). `unknown` is dotted, not green: §19.2 is
 * explicit — "never a false running". Stale observed data must never be shown
 * as healthy.
 */
const RESOURCE: Record<string, StatusMeaning> = {
  running: { family: 'success', modifier: 'none' },
  healthy: { family: 'success', modifier: 'none' },
  ready: { family: 'success', modifier: 'none' },
  starting: { family: 'progress', modifier: 'none' },
  pending: { family: 'progress', modifier: 'none' },
  validating: { family: 'progress', modifier: 'none' },
  deleting: { family: 'progress', modifier: 'none' },
  unhealthy: { family: 'warning', modifier: 'none' },
  maintenance: { family: 'warning', modifier: 'none' },
  degraded: { family: 'warning', modifier: 'none' },
  unreachable: { family: 'danger', modifier: 'none' },
  missing: { family: 'danger', modifier: 'none' },
  stopped: { family: 'neutral', modifier: 'none' },
  exited: { family: 'neutral', modifier: 'none' },
  deleted: { family: 'neutral', modifier: 'none' },
  unknown: { family: 'neutral', modifier: 'stale' },
};

/** Job states (§21.3). */
const JOB: Record<string, StatusMeaning> = {
  scheduled: { family: 'progress', modifier: 'none' },
  queued: { family: 'progress', modifier: 'none' },
  leased: { family: 'progress', modifier: 'none' },
  running: { family: 'progress', modifier: 'none' },
  retry_wait: { family: 'progress', modifier: 'none' },
  succeeded: { family: 'success', modifier: 'none' },
  // There is no `failed` job state: a failed attempt returns to retry_wait, and
  // becomes dead_letter once the attempts are exhausted (§21.3).
  dead_letter: { family: 'danger', modifier: 'none' },
  cancelled: { family: 'neutral', modifier: 'superseded' },
};

/**
 * Scheduled task executions (§192). `skipped` is neutral-but-marked, NOT a
 * success: an occurrence that did not run is not an occurrence that went fine,
 * and the operator has to be able to tell them apart at a glance.
 */
const TASK: Record<string, StatusMeaning> = {
  running: { family: 'progress', modifier: 'none' },
  succeeded: { family: 'success', modifier: 'none' },
  failed: { family: 'danger', modifier: 'none' },
  skipped: { family: 'neutral', modifier: 'superseded' },
};

/**
 * PR preview states (§20.4). `destroying`/`waking` are transitional and animate;
 * `cleanup_failed` is a warning — the teardown left resources behind and needs
 * attention, distinct from the preview itself having failed to deploy;
 * `destroyed` is a clean terminal end, neutral like a stopped resource.
 * `sleeping` (scale-to-zero, ADR-036) is a DELIBERATE stop, not a `down`: it is
 * neutral, and wakes on the first request — never shown as a failure.
 */
const PREVIEW: Record<string, StatusMeaning> = {
  queued: { family: 'progress', modifier: 'none' },
  deploying: { family: 'progress', modifier: 'none' },
  active: { family: 'success', modifier: 'none' },
  failed: { family: 'danger', modifier: 'none' },
  destroying: { family: 'progress', modifier: 'none' },
  cleanup_failed: { family: 'warning', modifier: 'none' },
  destroyed: { family: 'neutral', modifier: 'none' },
  sleeping: { family: 'neutral', modifier: 'none' },
  waking: { family: 'progress', modifier: 'none' },
};

export type StatusDomain = 'deployment' | 'resource' | 'job' | 'task' | 'preview';

const TABLES: Record<StatusDomain, Record<string, StatusMeaning>> = {
  deployment: DEPLOYMENT,
  resource: RESOURCE,
  job: JOB,
  task: TASK,
  preview: PREVIEW,
};

/**
 * Resolves a state to its family. An UNKNOWN state falls back to neutral+stale
 * rather than to success: an unmapped state is, by definition, a state we do
 * not understand, and showing it as healthy would be the exact failure §19.2
 * forbids. The spec test asserts the tables are complete, so this fallback
 * should never fire in practice — it is there for the day the API grows a state
 * the UI has not learned yet.
 */
export function statusMeaning(domain: StatusDomain, state: string): StatusMeaning {
  return TABLES[domain][state] ?? { family: 'neutral', modifier: 'stale' };
}

export function knownStates(domain: StatusDomain): string[] {
  return Object.keys(TABLES[domain]);
}
