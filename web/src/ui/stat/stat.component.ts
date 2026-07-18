import { ChangeDetectionStrategy, Component, input } from '@angular/core';

/** KPI figure with micro-label (design-system §3.9). */
@Component({
  selector: 'akd-stat',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-stat__label">{{ label() }}</div>
    <div class="akd-stat__value">
      {{ value() }}
      @if (unit()) {
        <span class="akd-stat__unit">{{ unit() }}</span>
      }
    </div>
    @if (delta()) {
      <div class="akd-stat__delta">{{ delta() }}</div>
    }
  `,
  styles: [
    `
      :host {
        display: block;
      }
    `,
  ],
})
export class StatComponent {
  readonly label = input.required<string>();
  readonly value = input.required<string | number>();
  readonly unit = input<string | undefined>(undefined);
  readonly delta = input<string | undefined>(undefined);
}
