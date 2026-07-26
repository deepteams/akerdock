import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  effect,
  input,
  output,
  viewChild,
} from '@angular/core';
import { IconComponent } from '../icon/icon.component';

/**
 * Right-side slide-over drawer (design-system §3.12, sibling of the modal).
 * Built on <dialog> so focus trapping, Escape and ::backdrop come from the
 * platform; pinned to the right edge and full height via the akd-drawer styles.
 * Use for a focused create/edit form that would otherwise clutter the page.
 */
@Component({
  selector: 'akd-drawer',
  standalone: true,
  imports: [IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <dialog #dialog class="akd-drawer" (close)="closed.emit()" (cancel)="closed.emit()">
      <div class="akd-drawer__header">
        <span>{{ title() }}</span>
        <span class="spacer"></span>
        <button
          type="button"
          class="akd-btn akd-btn--ghost akd-btn--sm"
          aria-label="Close"
          (click)="closed.emit()"
        >
          <akd-icon name="x" [size]="16" />
        </button>
      </div>
      <div class="akd-drawer__body"><ng-content /></div>
      <div class="akd-drawer__footer"><ng-content select="[drawer-footer]" /></div>
    </dialog>
  `,
})
export class DrawerComponent {
  readonly open = input.required<boolean>();
  readonly title = input.required<string>();
  readonly closed = output<void>();

  private readonly dialog = viewChild.required<ElementRef<HTMLDialogElement>>('dialog');

  constructor() {
    effect(() => {
      const el = this.dialog().nativeElement;
      if (this.open() && !el.open) el.showModal();
      else if (!this.open() && el.open) el.close();
    });
  }
}
