import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type LogLine = components['schemas']['LogLine'];

/**
 * Runtime console of the application's container (§5.7): the deployment logs
 * stop at the switch — everything the app prints after lives only in
 * `docker logs`, and this tab is its window. Snapshot + optional follow
 * (poll), never stored.
 */
@Component({
  selector: 'app-application-logs-tab',
  standalone: true,
  imports: [FormsModule, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <akd-card title="Container logs" [padded]="false">
      <div class="toolbar">
        <div class="akd-select">
          <select name="lines" class="akd-input" [(ngModel)]="lines" (ngModelChange)="refresh()">
            <option [ngValue]="200">Last 200 lines</option>
            <option [ngValue]="500">Last 500 lines</option>
            <option [ngValue]="2000">Last 2000 lines</option>
          </select>
        </div>
        <label class="akd-check">
          <input type="checkbox" name="follow" [(ngModel)]="follow" (ngModelChange)="onFollow()" />
          Follow (refresh every 3 s)
        </label>
        <span class="spacer"></span>
        <button
          class="akd-btn akd-btn--secondary akd-btn--sm"
          type="button"
          [disabled]="busy()"
          (click)="refresh()"
        >
          <akd-icon name="refresh-cw" [size]="13" />
          Refresh
        </button>
      </div>
      @if (logs(); as lines) {
        @if (lines.length === 0) {
          <p class="akd-muted empty">The container has not written anything yet.</p>
        } @else {
          <pre class="log"><code>@for (line of lines; track line.sequence) {{{ line.message }}
}</code></pre>
        }
      } @else if (busy()) {
        <p class="akd-muted empty">Loading…</p>
      }
    </akd-card>
  `,
  styles: [
    `
      .toolbar {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-3);
        border-bottom: 1px solid var(--border);
      }
      .spacer {
        flex: 1;
      }
      .empty {
        padding: var(--space-3);
      }
      .log {
        margin: 0;
        padding: var(--space-3);
        max-height: 60vh;
        overflow: auto;
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        line-height: 1.5;
        white-space: pre-wrap;
        word-break: break-all;
        /* The log surface is dark in both themes (design-system §2.6). */
        background: var(--log-bg, var(--surface-2));
      }
    `,
  ],
})
export class ApplicationLogsTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly logs = signal<LogLine[] | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected lines = 200;
  protected follow = false;
  private timer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
    inject(DestroyRef).onDestroy(() => this.stopFollow());
  }

  protected refresh(): void {
    void this.load(this.uuid());
  }

  protected onFollow(): void {
    this.stopFollow();
    if (this.follow) {
      this.timer = setInterval(() => void this.load(this.uuid()), 3000);
    }
  }

  private stopFollow(): void {
    if (this.timer !== null) {
      clearInterval(this.timer);
      this.timer = null;
    }
  }

  private async load(uuid: string): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      const page = await this.api.client().getApplicationLogs(uuid, { lines: this.lines });
      this.logs.set(page.data);
      this.error.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      // A dead container is a stable answer, not a reason to hammer the API.
      this.follow = false;
      this.stopFollow();
    } finally {
      this.busy.set(false);
    }
  }
}
