import { ChangeDetectionStrategy, Component, input } from '@angular/core';
import { RouterLink } from '@angular/router';

export interface Crumb {
  label: string;
  /** Router target; the last crumb usually has none (it is the current page). */
  link?: unknown[] | string;
}

/** Location trail in the topbar and page headers (design-system §3.7). */
@Component({
  selector: 'akd-breadcrumb',
  standalone: true,
  imports: [RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <nav class="akd-breadcrumb" aria-label="Breadcrumb">
      @for (crumb of items(); track $index; let last = $last) {
        @if (crumb.link && !last) {
          <a [routerLink]="crumb.link">{{ crumb.label }}</a>
        } @else {
          <span [class.akd-breadcrumb__current]="last" [attr.aria-current]="last ? 'page' : null">
            {{ crumb.label }}
          </span>
        }
        @if (!last) {
          <span class="akd-breadcrumb__sep" aria-hidden="true">/</span>
        }
      }
    </nav>
  `,
})
export class BreadcrumbComponent {
  readonly items = input.required<Crumb[]>();
}
