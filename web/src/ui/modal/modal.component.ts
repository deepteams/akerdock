import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  effect,
  input,
  output,
  viewChild,
} from '@angular/core';

/**
 * Modal dialog (design-system §3.12). Rendered on <dialog> so focus trapping,
 * Escape and ::backdrop come from the platform, not from a re-implementation.
 * `danger` tints the border for destructive confirmations — the visual warning
 * appears BEFORE the click, per principle §1.4.
 */
@Component({
  selector: 'akd-modal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <dialog
      #dialog
      class="akd-modal"
      [class.akd-modal--danger]="danger()"
      (close)="closed.emit()"
      (cancel)="closed.emit()"
    >
      <div class="akd-modal__header">{{ title() }}</div>
      <div class="akd-modal__body"><ng-content /></div>
      <div class="akd-modal__footer"><ng-content select="[modal-footer]" /></div>
    </dialog>
  `,
  styles: [
    `
      dialog {
        padding: 0;
        color: inherit;
      }
      dialog::backdrop {
        background: oklch(10% 0.01 252 / 0.6);
        backdrop-filter: blur(4px);
      }
      dialog[open] {
        animation: akd-slide-in var(--dur-2) var(--ease-out);
      }
    `,
  ],
})
export class ModalComponent {
  readonly open = input.required<boolean>();
  readonly title = input.required<string>();
  readonly danger = input<boolean>(false);
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
