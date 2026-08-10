import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';

import { IconComponent } from '../icon/icon.component';
import { filterTimeZones, formatOffset, timeZoneOptions } from './timezone';

/** Rough panel height in px — the room the list needs to open downwards. */
const PANEL_HEIGHT = 300;

/**
 * Timezone picker for anything that carries a cron (§24.3): scheduled tasks and
 * backup plans. The value is the IANA name and nothing else — that is what the
 * API validates and what both schedulers compute occurrences in.
 *
 * Two decisions are worth stating. First, every entry shows its **current** UTC
 * offset, because the question the operator is really asking is "at what hour
 * will this fire", and `Europe/Paris` alone does not answer it in a DST year.
 * Second, it is a filter box and not a bare `<select>`: the engine's database
 * holds 400+ zones, and a native dropdown that long is a scroll, not a choice.
 *
 * Two-way bindable: `[(value)]="timezone"`.
 */
@Component({
  selector: 'akd-timezone-select',
  standalone: true,
  imports: [IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  // Same dismissal contract as akd-actions-menu: any click outside closes it,
  // and the toggle stops propagation so opening does not immediately close.
  host: { '(document:click)': 'close()', '(document:keydown.escape)': 'close()' },
  template: `
    <div class="wrap">
      <button
        class="akd-input trigger"
        type="button"
        [attr.id]="inputId()"
        [disabled]="disabled()"
        [attr.aria-expanded]="open()"
        aria-haspopup="listbox"
        (click)="toggle($event)"
      >
        <akd-icon name="globe" [size]="14" />
        <span class="zone">{{ value() }}</span>
        <span class="offset">{{ offset() }}</span>
      </button>

      @if (open()) {
        <div class="panel" [class.panel--up]="dropUp()" (click)="$event.stopPropagation()">
          <input
            class="akd-input search"
            type="text"
            autocomplete="off"
            placeholder="Filter zones…"
            aria-label="Filter timezones"
            [value]="query()"
            (input)="onQuery($event)"
            (keydown.enter)="takeFirst($event)"
          />
          <div class="list" role="listbox" aria-label="Timezones">
            @for (option of visible(); track option.zone) {
              <button
                class="option"
                type="button"
                role="option"
                [class.option--on]="option.zone === value()"
                [attr.aria-selected]="option.zone === value()"
                (click)="pick(option.zone)"
              >
                <span class="zone">{{ option.zone }}</span>
                <span class="offset">{{ option.offset }}</span>
              </button>
            } @empty {
              <p class="empty akd-muted">No timezone matches “{{ query() }}”.</p>
            }
          </div>
        </div>
      }
    </div>
  `,
  styles: [
    `
      .wrap {
        position: relative;
        display: block;
      }
      .trigger {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        text-align: left;
        cursor: pointer;
      }
      .trigger akd-icon {
        color: var(--text-3);
        flex: none;
      }
      .zone {
        flex: 1;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .offset {
        color: var(--text-3);
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        flex: none;
      }
      .panel {
        position: absolute;
        top: calc(100% + 6px);
        left: 0;
        right: 0;
        z-index: 50;
        padding: 6px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      .panel--up {
        top: auto;
        bottom: calc(100% + 6px);
      }
      .search {
        margin-bottom: 6px;
      }
      .list {
        max-height: 15rem;
        overflow-y: auto;
      }
      .option {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        width: 100%;
        padding: 6px 10px;
        border: 0;
        border-radius: var(--radius-2);
        background: none;
        color: var(--text-1);
        font-size: var(--text-md);
        text-align: left;
        cursor: pointer;
      }
      .option:hover {
        background: var(--bg-2);
      }
      .option:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .option--on {
        color: var(--accent);
      }
      .empty {
        padding: 8px 10px;
        margin: 0;
        font-size: var(--text-sm);
      }
    `,
  ],
})
export class TimezoneSelectComponent {
  /** IANA name currently selected. Two-way bindable through `valueChange`. */
  readonly value = input<string>('UTC');
  readonly disabled = input<boolean>(false);
  /** Id put on the trigger so a sibling `<label for>` reaches it. */
  readonly inputId = input<string | null>(null);
  readonly valueChange = output<string>();

  protected readonly open = signal(false);
  protected readonly query = signal('');
  protected readonly dropUp = signal(false);
  /** Offsets are read at open time so a DST change is never shown stale. */
  private readonly now = signal(new Date());

  /**
   * The stored value is passed as `extra`: a zone this engine's database does
   * not carry stays in the list and stays selectable, so opening an edit form
   * can never rewrite what was saved.
   */
  protected readonly options = computed(() => timeZoneOptions(this.value(), this.now()));
  protected readonly visible = computed(() => filterTimeZones(this.options(), this.query()));
  protected readonly offset = computed(() => formatOffset(this.value(), this.now()));

  protected toggle(event: Event): void {
    event.stopPropagation();
    if (this.disabled()) return;
    const opening = !this.open();
    if (opening) {
      this.query.set('');
      this.now.set(new Date());
      this.dropUp.set(this.noRoomBelow(event.currentTarget));
    }
    this.open.set(opening);
  }

  /**
   * The field often sits at the bottom of a drawer whose body scrolls; a panel
   * dropped downwards there opens off-screen. Measured at open time rather than
   * guessed from the layout.
   */
  private noRoomBelow(trigger: EventTarget | null): boolean {
    if (!(trigger instanceof HTMLElement)) return false;
    const rect = trigger.getBoundingClientRect();
    const below = window.innerHeight - rect.bottom;
    return below < PANEL_HEIGHT && rect.top > below;
  }

  protected close(): void {
    this.open.set(false);
  }

  protected onQuery(event: Event): void {
    this.query.set((event.target as HTMLInputElement).value);
  }

  /** Enter picks the first match instead of submitting the surrounding form. */
  protected takeFirst(event: Event): void {
    event.preventDefault();
    const first = this.visible()[0];
    if (first) this.pick(first.zone);
  }

  protected pick(zone: string): void {
    this.open.set(false);
    this.query.set('');
    if (zone !== this.value()) this.valueChange.emit(zone);
  }
}
