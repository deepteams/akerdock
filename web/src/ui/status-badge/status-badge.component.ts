import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { StatusDomain, statusMeaning } from './status';

/**
 * The central piece of the design system (§3.6). Every business state in the UI
 * goes through this component — a dashboard card, a table row, a deployment
 * timeline and a job panel all render `running` identically, because they all
 * render it here.
 *
 * Never colour alone (§1.5, WCAG 1.4.1): the badge always shows a dot, a shape
 * modifier when relevant, AND the label. A colour-blind operator, a
 * black-and-white screenshot in an incident report, and a screen reader all get
 * the same information.
 */
@Component({
  selector: 'akd-status-badge',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span
      class="badge"
      [class]="'family-' + meaning().family + ' mod-' + meaning().modifier"
      role="status"
      [attr.aria-label]="ariaLabel()"
    >
      <span class="dot" aria-hidden="true"></span>
      <span class="label">{{ label() }}</span>
    </span>
  `,
  styles: [
    `
      /* Tokens only — no literal colour, size or duration (§6.1). */
      .badge {
        display: inline-flex;
        align-items: center;
        gap: var(--akd-space-1);
        padding: var(--akd-space-05) var(--akd-space-2);
        border-radius: var(--akd-radius-full);
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-medium);
        line-height: 1.4;
        white-space: nowrap;
        border: 1px solid transparent;
      }
      .dot {
        width: var(--akd-space-2);
        height: var(--akd-space-2);
        border-radius: var(--akd-radius-full);
        flex: none;
      }
      .family-success {
        color: var(--akd-status-success-fg);
        background: var(--akd-status-success-bg);
      }
      .family-success .dot {
        background: var(--akd-status-success-dot);
      }
      .family-progress {
        color: var(--akd-status-progress-fg);
        background: var(--akd-status-progress-bg);
      }
      .family-progress .dot {
        background: var(--akd-status-progress-dot);
        animation: pulse var(--akd-duration-slow) var(--akd-ease) infinite alternate;
      }
      .family-warning {
        color: var(--akd-status-warning-fg);
        background: var(--akd-status-warning-bg);
      }
      .family-warning .dot {
        background: var(--akd-status-warning-dot);
      }
      .family-danger {
        color: var(--akd-status-danger-fg);
        background: var(--akd-status-danger-bg);
      }
      .family-danger .dot {
        background: var(--akd-status-danger-dot);
      }
      .family-neutral {
        color: var(--akd-status-neutral-fg);
        background: var(--akd-status-neutral-bg);
      }
      .family-neutral .dot {
        background: var(--akd-status-neutral-dot);
      }

      /* Shape, not colour: readable without colour perception (§1.5). */
      .mod-stale {
        border-style: dashed;
        border-color: currentColor;
      }
      .mod-stale .dot {
        background: transparent;
        box-shadow: inset 0 0 0 1px currentColor;
      }
      .mod-superseded .label {
        text-decoration: line-through;
      }

      @keyframes pulse {
        from {
          opacity: 1;
        }
        to {
          opacity: 0.45;
        }
      }
      /* prefers-reduced-motion collapses the durations in tokens.css, but an
         infinite animation must stop outright, not merely run fast. */
      @media (prefers-reduced-motion: reduce) {
        .family-progress .dot {
          animation: none;
        }
      }
    `,
  ],
})
export class StatusBadgeComponent {
  /** Which state machine the value belongs to (§21.1/§21.2/§21.3). */
  readonly domain = input.required<StatusDomain>();
  readonly state = input.required<string>();
  /** Overrides the label; defaults to the state itself, humanised. */
  readonly labelOverride = input<string | undefined>(undefined, { alias: 'label' });

  protected readonly meaning = computed(() => statusMeaning(this.domain(), this.state()));

  protected readonly label = computed(
    () => this.labelOverride() ?? this.state().replace(/_/g, ' '),
  );

  protected readonly ariaLabel = computed(() => {
    const meaning = this.meaning();
    // The modifier carries meaning a sighted user reads from the shape; a screen
    // reader must be told in words.
    const suffix =
      meaning.modifier === 'stale'
        ? ' (stale — last known state, may be out of date)'
        : meaning.modifier === 'superseded'
          ? ' (superseded)'
          : '';
    return `${this.label()}${suffix}`;
  });
}
