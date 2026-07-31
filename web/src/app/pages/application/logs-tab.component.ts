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
type ServiceComponent = components['schemas']['ServiceComponent'];

/**
 * Runtime console of the application's container (§5.7): the deployment logs
 * stop at the switch — everything the app prints after lives only in
 * `docker logs`, and this tab is its window. Snapshot + optional follow over
 * SSE (the same stream `akerdock logs -f` rides), never stored.
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
        @if (components().length > 0) {
          <!-- A compose stack is several containers: pick whose console. -->
          <div class="akd-select">
            <select
              name="component"
              class="akd-input"
              [(ngModel)]="component"
              (ngModelChange)="refresh()"
            >
              @for (c of components(); track c.name) {
                <option [ngValue]="c.name">{{ c.name }}</option>
              }
            </select>
          </div>
        }
        <div class="akd-select">
          <select name="lines" class="akd-input" [(ngModel)]="lines" (ngModelChange)="refresh()">
            <option [ngValue]="200">Last 200 lines</option>
            <option [ngValue]="500">Last 500 lines</option>
            <option [ngValue]="2000">Last 2000 lines</option>
          </select>
        </div>
        <label class="akd-check">
          <input type="checkbox" name="follow" [(ngModel)]="follow" (ngModelChange)="onFollow()" />
          Follow (live)
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
  /** Compose service to open on load — set when the overview deep-links here. */
  readonly preselect = input<string>('');

  private readonly api = inject(ApiService);

  protected readonly logs = signal<LogLine[] | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  /** Compose services of the stack — empty for single-container packs. */
  protected readonly components = signal<ServiceComponent[]>([]);

  protected lines = 200;
  protected component = '';
  protected follow = false;
  private source: EventSource | null = null;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.init(uuid));
    });
    inject(DestroyRef).onDestroy(() => this.stopFollow());
  }

  /** Components first: a compose stack has no container of its own — the
   * first fetch must already name a service. */
  private async init(uuid: string): Promise<void> {
    try {
      const page = await this.api.client().listApplicationComponents(uuid);
      this.components.set(page.data);
      if (page.data.length > 0) {
        // Honour the component the overview deep-linked to, else the first one.
        const wanted = this.preselect();
        const match = page.data.find((c) => c.name === wanted);
        this.component = match ? match.name : page.data[0].name;
      }
    } catch {
      this.components.set([]);
    }
    await this.load(uuid);
  }

  protected refresh(): void {
    // A component or window change while following restarts the stream on the
    // new selection; otherwise it is a plain snapshot reload.
    if (this.follow) {
      this.onFollow();
      return;
    }
    void this.load(this.uuid());
  }

  protected onFollow(): void {
    this.stopFollow();
    if (this.follow) {
      this.openStream(this.uuid());
    }
  }

  /**
   * Live follow over the SSE stream (ADR-024). The server tails the last 200
   * lines into the stream, so it replaces the snapshot rather than topping it
   * up; lines are keyed by sequence, making a reconnect's replay idempotent.
   */
  private openStream(uuid: string): void {
    this.logs.set(null);
    this.busy.set(true);
    const source = this.api
      .client()
      .streamApplicationLogs(uuid, this.component ? { component: this.component } : undefined);
    this.source = source;

    source.addEventListener('log', (event) => {
      const line = JSON.parse((event as MessageEvent<string>).data) as LogLine;
      this.busy.set(false);
      this.error.set(null);
      this.logs.update((lines) => {
        const next = (lines ?? []).filter((l) => l.sequence !== line.sequence);
        next.push(line);
        next.sort((a, b) => a.sequence - b.sequence);
        // Cap what the DOM holds: a chatty container must not grow the page
        // without bound. 2000 matches the deepest snapshot window.
        return next.slice(-2000);
      });
    });
    source.addEventListener('open', () => this.busy.set(false));
    // EventSource retries on its own; surface the interruption without
    // tearing the follow down.
    source.addEventListener('error', () => {
      if (this.follow) this.error.set('Log stream interrupted — reconnecting…');
    });
  }

  private stopFollow(): void {
    this.source?.close();
    this.source = null;
  }

  private async load(uuid: string): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      const page = await this.api.client().getApplicationLogs(uuid, {
        lines: this.lines,
        ...(this.component ? { component: this.component } : {}),
      });
      this.logs.set(page.data);
      this.error.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
