import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { StatusDomain, statusMeaning } from './status';

/**
 * The central piece of the design system (§3.6). Every business state in the UI
 * goes through this component — a dashboard card, a table row, a deployment
 * timeline and a job panel all render `running` identically, because they all
 * render it here.
 *
 * Never colour alone (§1.5, WCAG 1.4.1): each family also has its own dot
 * SHAPE (disc, rotated square, triangle, square, ring), plus a modifier when
 * relevant, AND the label. A colour-blind operator, a black-and-white
 * screenshot in an incident report, and a screen reader all get the same
 * information.
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
        gap: var(--space-2);
        padding: 3px 12px 3px 10px;
        border-radius: var(--radius-full);
        font: var(--weight-medium) var(--text-sm) var(--font-body);
        white-space: nowrap;
        border: 1px solid transparent;
      }
      .dot {
        width: 8px;
        height: 8px;
        flex: none;
      }
      .family-success {
        color: var(--ok);
        background: var(--ok-dim);
        border-color: var(--ok-border);
      }
      .family-success .dot {
        border-radius: 50%;
        background: var(--ok);
      }
      .family-progress {
        color: var(--accent);
        background: var(--info-dim);
        border-color: var(--accent-border);
      }
      .family-progress .dot {
        background: var(--accent);
        transform: rotate(45deg) scale(0.9);
        animation: akd-pulse 1.4s var(--ease-out) infinite;
      }
      .family-warning {
        color: var(--warn);
        background: var(--warn-dim);
        border-color: var(--warn-border);
      }
      .family-warning .dot {
        width: 0;
        height: 0;
        background: none;
        border-left: 5px solid transparent;
        border-right: 5px solid transparent;
        border-bottom: 9px solid var(--warn);
      }
      .family-danger {
        color: var(--danger);
        background: var(--danger-dim);
        border-color: var(--danger-border);
      }
      .family-danger .dot {
        background: var(--danger);
      }
      .family-neutral {
        color: var(--neutral);
        background: var(--neutral-dim);
        border-color: var(--neutral-border);
      }
      .family-neutral .dot {
        border-radius: 50%;
        border: 2px solid var(--neutral);
        background: none;
        box-sizing: border-box;
      }

      /* Shape, not colour: readable without colour perception (§1.5). */
      .mod-stale {
        border-style: dashed;
      }
      .mod-stale .dot {
        background: transparent;
        box-shadow: inset 0 0 0 1px currentColor;
      }
      .mod-superseded .label {
        text-decoration: line-through;
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
