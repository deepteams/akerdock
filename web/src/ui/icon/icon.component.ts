import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { LucideAngularModule } from 'lucide-angular';
import { ICONS } from './icons';

/**
 * Decorative icon (design-system §3.1). Names come from the design kit's
 * lucide vocabulary; the registry in icons.ts is the allow-list. Icons are
 * always aria-hidden: the adjacent text carries the meaning, never the glyph.
 */
@Component({
  selector: 'akd-icon',
  standalone: true,
  imports: [LucideAngularModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<lucide-icon [img]="img()" [size]="size()" [strokeWidth]="2" aria-hidden="true" />`,
  styles: [
    `
      :host {
        display: inline-flex;
        flex: none;
        vertical-align: -2px;
        line-height: 0;
      }
    `,
  ],
})
export class IconComponent {
  readonly name = input.required<string>();
  readonly size = input<number>(16);

  // Falling back to `circle` makes a missing registry entry visible in the UI
  // (a plain dot where a glyph should be) instead of throwing.
  protected readonly img = computed(() => ICONS[this.name()] ?? ICONS['circle']);
}
