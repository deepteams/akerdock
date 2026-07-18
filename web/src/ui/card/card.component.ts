import { ChangeDetectionStrategy, Component, input } from '@angular/core';

/**
 * Surface container (design-system §3.5). With a title it renders the kit's
 * header row (display-font title, optional actions via the `card-actions`
 * slot); `padded=false` lets a table bleed to the card edges.
 */
@Component({
  selector: 'akd-card',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-card">
      @if (title()) {
        <div class="akd-card__header">
          <h2 class="akd-card__title">{{ title() }}</h2>
          <div class="spacer"></div>
          <ng-content select="[card-actions]" />
        </div>
      }
      <!-- Single ng-content on purpose: Angular instantiates a duplicated
           default slot only once, so an @if/@else pair of slots leaves one
           branch permanently empty. -->
      <div [class.akd-card__body]="padded()"><ng-content /></div>
    </div>
  `,
  styles: [
    `
      :host {
        display: block;
        min-width: 0;
      }
      .spacer {
        flex: 1;
      }
    `,
  ],
})
export class CardComponent {
  readonly title = input<string | undefined>(undefined);
  readonly padded = input<boolean>(true);
}
