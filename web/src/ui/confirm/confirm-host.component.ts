import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { ModalComponent } from '../modal/modal.component';
import { ConfirmService } from './confirm.service';

/** Renders the ConfirmService's pending question. Mounted once, in the shell. */
@Component({
  selector: 'akd-confirm-host',
  standalone: true,
  imports: [ModalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (confirm.pending(); as pending) {
      <akd-modal
        [open]="true"
        [title]="pending.title"
        [danger]="pending.danger"
        (closed)="confirm.settle(false)"
      >
        <p class="akd-muted">{{ pending.message }}</p>
        <div modal-footer>
          <button class="akd-btn akd-btn--ghost" type="button" (click)="confirm.settle(false)">
            Cancel
          </button>
          <button
            [class]="pending.danger ? 'akd-btn akd-btn--danger' : 'akd-btn akd-btn--primary'"
            type="button"
            (click)="confirm.settle(true)"
          >
            {{ pending.confirmLabel }}
          </button>
        </div>
      </akd-modal>
    }
  `,
})
export class ConfirmHostComponent {
  protected readonly confirm = inject(ConfirmService);
}
