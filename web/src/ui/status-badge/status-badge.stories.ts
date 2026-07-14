// The catalogue entry of the design system's central piece (§3.6): every
// business state, rendered by the ONE component that owns the state → family
// table. If a state looks wrong here, it looks wrong everywhere — which is
// the point.
import type { Meta, StoryObj } from '@storybook/angular';
import { StatusBadgeComponent } from './status-badge.component';

const meta: Meta<StatusBadgeComponent> = {
  title: 'Design System/StatusBadge',
  component: StatusBadgeComponent,
  argTypes: {
    domain: { control: 'select', options: ['deployment', 'resource', 'job', 'task'] },
    state: { control: 'text' },
  },
};
export default meta;

type Story = StoryObj<StatusBadgeComponent>;

export const Playground: Story = {
  args: { domain: 'deployment', state: 'succeeded' },
};

// One story per §21 state machine: the exhaustive truth table, visually.
// Colour is never alone (WCAG 1.4.1) — dot + shape + label survive a
// black-and-white screenshot, which is exactly what addon-a11y verifies.
const gallery = (domain: string, states: string[]) => ({
  render: () => ({
    props: { domain, states },
    template: `
      <div style="display: flex; flex-wrap: wrap; gap: var(--akd-space-2); max-width: 640px;">
        @for (s of states; track s) {
          <akd-status-badge [domain]="domain" [state]="s" />
        }
      </div>
    `,
    moduleMetadata: { imports: [StatusBadgeComponent] },
  }),
});

export const DeploymentStates: Story = gallery('deployment', [
  'queued', 'preparing', 'cloning', 'building', 'pushing', 'starting',
  'healthchecking', 'switching', 'finishing', 'retrying',
  'succeeded', 'failed', 'cancelled', 'superseded',
]);

export const ResourceAndServerStates: Story = gallery('resource', [
  'running', 'healthy', 'ready', 'starting', 'pending', 'validating',
  'deleting', 'unhealthy', 'maintenance', 'degraded', 'unreachable',
  'missing', 'stopped', 'exited', 'deleted', 'unknown',
]);

export const JobStates: Story = gallery('job', [
  'scheduled', 'queued', 'leased', 'running', 'retry_wait',
  'succeeded', 'cancelled', 'dead_letter',
]);

export const TaskStates: Story = gallery('task', [
  'running', 'succeeded', 'failed', 'skipped',
]);
