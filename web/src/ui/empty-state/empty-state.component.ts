import { ChangeDetectionStrategy, Component, input } from '@angular/core';
import { IconComponent } from '../icon/icon.component';

/**
 * Empty state (design-system §3.11): an icon, a display-font title, a short
 * explanation, and optionally a call to action projected as content.
 */
@Component({
  selector: 'akd-empty-state',
  standalone: true,
  imports: [IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-empty">
      <span class="akd-empty__icon"><akd-icon [name]="icon()" [size]="28" /></span>
      <div class="akd-empty__title">{{ title() }}</div>
      @if (message()) {
        <div class="akd-empty__msg">{{ message() }}</div>
      }
      <ng-content />
    </div>
  `,
})
export class EmptyStateComponent {
  readonly icon = input<string>('box');
  readonly title = input.required<string>();
  readonly message = input<string | undefined>(undefined);
}
