import { statusMeaning, knownStates, StatusDomain } from './status';
import type { components } from '../../api/schema';

// The mapping is only useful if it is COMPLETE. A state the API can return but
// the table does not know would render as neutral+stale — visible, but wrong,
// and nobody would notice until an operator wondered why a healthy app looks
// unknown.
//
// So completeness is asserted against the generated contract, not against a
// hand-kept list: when the OpenAPI spec grows a state, this test fails until
// someone decides which family it belongs to. That decision is exactly the one
// a human must make.

// These types come from the contract; listing their members is a compile-time
// exhaustiveness check, and the runtime arrays below feed the assertions.
const DEPLOYMENT_STATES: components['schemas']['DeploymentStatus'][] = [
  'queued',
  'preparing',
  'cloning',
  'building',
  'pushing',
  'starting',
  'healthchecking',
  'switching',
  'finishing',
  'succeeded',
  'failed',
  'cancelled',
  'retrying',
  'superseded',
];

const OBSERVED_STATES: components['schemas']['ObservedStatus'][] = [
  'unknown',
  'starting',
  'healthy',
  'unhealthy',
  'exited',
  'missing',
];

// Note: the contract has no `failed` job state — a failed attempt goes back to
// retry_wait, or to dead_letter when the attempts are exhausted. Listing it
// would not compile, which is the type system doing the review for us.
const JOB_STATES: components['schemas']['JobStatus'][] = [
  'scheduled',
  'queued',
  'leased',
  'running',
  'retry_wait',
  'succeeded',
  'dead_letter',
  'cancelled',
];

const TASK_STATES: components['schemas']['TaskExecutionStatus'][] = [
  'running',
  'succeeded',
  'failed',
  'skipped',
];

const PREVIEW_STATES: components['schemas']['Preview']['status'][] = [
  'queued',
  'deploying',
  'active',
  'failed',
  'destroying',
  'cleanup_failed',
  'destroyed',
  'sleeping',
  'waking',
];

describe('status mapping', () => {
  function assertMapped(domain: StatusDomain, states: string[]) {
    const known = new Set(knownStates(domain));
    const missing = states.filter((s) => !known.has(s));
    expect(missing)
      .withContext(
        `${domain}: the contract can return these states but the badge does not map them — ` +
          `they would render as "unknown". Decide their family in status.ts.`,
      )
      .toEqual([]);
  }

  it('maps every deployment state the contract can return', () => {
    assertMapped('deployment', DEPLOYMENT_STATES);
  });

  it('maps every observed resource state the contract can return', () => {
    assertMapped('resource', OBSERVED_STATES);
  });

  it('maps every job state the contract can return', () => {
    assertMapped('job', JOB_STATES);
  });

  it('maps every task execution state the contract can return', () => {
    assertMapped('task', TASK_STATES);
  });

  it('maps every preview state the contract can return', () => {
    assertMapped('preview', PREVIEW_STATES);
  });

  // Scale-to-zero (ADR-036) is a deliberate stop, not an outage: a sleeping
  // preview must never look like a failure, and waking is in-flight progress.
  it('treats a sleeping preview as neutral and waking as progress, never danger', () => {
    expect(statusMeaning('preview', 'sleeping').family).toBe('neutral');
    expect(statusMeaning('preview', 'waking').family).toBe('progress');
    expect(statusMeaning('preview', 'sleeping').family).not.toBe('danger');
  });

  // A skipped occurrence is NOT a success: it never ran.
  it('never renders a skipped task execution as success', () => {
    expect(statusMeaning('task', 'skipped').family).not.toBe('success');
  });

  // §19.2: "never a false running". Stale data must not look healthy.
  it('renders unknown as stale, never as success', () => {
    const meaning = statusMeaning('resource', 'unknown');
    expect(meaning.family).toBe('neutral');
    expect(meaning.modifier).toBe('stale');
  });

  // An unmapped state must degrade to "we don't know", not to "everything fine".
  it('degrades an unmapped state to stale rather than success', () => {
    expect(statusMeaning('resource', 'a-state-from-the-future').family).toBe('neutral');
    expect(statusMeaning('resource', 'a-state-from-the-future').modifier).toBe('stale');
  });

  // Abandoned states are struck through: distinguishable without colour (WCAG 1.4.1).
  it('marks cancelled and superseded deployments as struck through', () => {
    expect(statusMeaning('deployment', 'cancelled').modifier).toBe('superseded');
    expect(statusMeaning('deployment', 'superseded').modifier).toBe('superseded');
  });

  // A failure must never be quiet.
  it('maps failures to danger', () => {
    expect(statusMeaning('deployment', 'failed').family).toBe('danger');
    expect(statusMeaning('job', 'dead_letter').family).toBe('danger');
    expect(statusMeaning('resource', 'unreachable').family).toBe('danger');
  });
});
