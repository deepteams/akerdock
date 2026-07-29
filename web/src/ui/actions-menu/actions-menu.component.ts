import { ChangeDetectionStrategy, Component, input, output, signal } from '@angular/core';

import { IconComponent } from '../icon/icon.component';

/** One entry of the menu. `hint` is the line that says what it actually does. */
export interface ActionItem {
  id: string;
  label: string;
  icon: string;
  hint?: string;
  danger?: boolean;
  disabled?: boolean;
}

/**
 * Overflow menu for a resource's operations (design-system §3.5). A resource
 * has more verbs than a header can hold side by side — deploy, rebuild,
 * reapply the configuration, restart, stop — and lining them all up makes the
 * dangerous ones as prominent as the routine one. They live here instead,
 * each with a hint: "restart" and "recreate" are one word apart and worlds
 * apart in effect.
 */
@Component({
  selector: 'akd-actions-menu',
  standalone: true,
  imports: [IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  // Any click outside dismisses it — the toggle stops propagation so opening
  // the menu does not immediately close it.
  host: { '(document:click)': 'close()', '(document:keydown.escape)': 'close()' },
  template: `
    <div class="wrap">
      <button
        class="akd-btn akd-btn--secondary"
        type="button"
        [disabled]="disabled()"
        [attr.aria-expanded]="open()"
        aria-haspopup="menu"
        (click)="toggle($event)"
      >
        {{ label() }}
        <akd-icon name="chevron-down" [size]="14" />
      </button>

      @if (open()) {
        <div class="menu" role="menu">
          @for (item of items(); track item.id) {
            <button
              class="item"
              type="button"
              role="menuitem"
              [class.item--danger]="item.danger"
              [disabled]="item.disabled || disabled()"
              (click)="pick(item)"
            >
              <akd-icon [name]="item.icon" [size]="15" />
              <span class="text">
                <span class="label">{{ item.label }}</span>
                @if (item.hint) {
                  <span class="hint">{{ item.hint }}</span>
                }
              </span>
            </button>
          }
        </div>
      }
    </div>
  `,
  styles: [
    `
      .wrap {
        position: relative;
        display: inline-flex;
      }
      .menu {
        position: absolute;
        top: calc(100% + 6px);
        right: 0;
        z-index: 50;
        min-width: 260px;
        padding: 6px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      .item {
        display: flex;
        align-items: flex-start;
        gap: 10px;
        width: 100%;
        padding: 7px 10px;
        border: 0;
        border-radius: var(--radius-2);
        background: none;
        color: var(--text-1);
        text-align: left;
        cursor: pointer;
      }
      .item:hover:not(:disabled) {
        background: var(--bg-2);
      }
      .item:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .item:disabled {
        opacity: 0.5;
        cursor: not-allowed;
      }
      .item akd-icon {
        margin-top: 2px;
        color: var(--text-2);
      }
      .item--danger,
      .item--danger akd-icon {
        color: var(--danger);
      }
      .text {
        display: flex;
        flex-direction: column;
        gap: 1px;
      }
      .label {
        font-size: var(--text-md);
        font-weight: var(--weight-medium);
      }
      .hint {
        font-size: var(--text-2xs);
        color: var(--text-3);
      }
    `,
  ],
})
export class ActionsMenuComponent {
  readonly items = input.required<ActionItem[]>();
  readonly label = input<string>('Actions');
  readonly disabled = input<boolean>(false);
  readonly selected = output<string>();

  protected readonly open = signal(false);

  protected toggle(event: Event): void {
    event.stopPropagation();
    this.open.update((value) => !value);
  }

  protected close(): void {
    this.open.set(false);
  }

  protected pick(item: ActionItem): void {
    this.open.set(false);
    this.selected.emit(item.id);
  }
}
