import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import type { components } from '../../api/schema';

type AuditEvent = components['schemas']['AuditEvent'];

export interface AuditQuery {
  cursor?: string;
  limit?: number;
  action?: string;
  result?: 'success' | 'failure' | 'denied';
  actor_uuid?: string;
  from?: string;
  to?: string;
}
export interface AuditPage {
  data: AuditEvent[];
  next_cursor?: string | null;
}
export type AuditFetch = (query: AuditQuery) => Promise<AuditPage>;

/**
 * Reusable audit-log viewer: filters, a table and a CSV export. It is source-
 * agnostic — the parent passes a `fetch` function, so the same widget serves the
 * team-scoped trail (/teams/{uuid}/audit) and the instance-wide one
 * (/system/audit). The export paginates through EVERY matching row (following
 * next_cursor), not just the page on screen.
 */
@Component({
  selector: 'akd-audit-log',
  standalone: true,
  imports: [FormsModule, SlicePipe, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="filters">
      <input
        class="akd-input akd-input--mono"
        placeholder="action (e.g. auth.login)"
        [(ngModel)]="fAction"
        (keyup.enter)="apply()"
      />
      <div class="akd-select">
        <select class="akd-input" [(ngModel)]="fResult">
          <option value="">any result</option>
          <option value="success">success</option>
          <option value="failure">failure</option>
          <option value="denied">denied</option>
        </select>
      </div>
      <input class="akd-input" type="date" [(ngModel)]="fFrom" aria-label="From date" />
      <input class="akd-input" type="date" [(ngModel)]="fTo" aria-label="To date" />
      <button
        class="akd-btn akd-btn--ghost akd-btn--sm"
        type="button"
        [disabled]="loading()"
        (click)="apply()"
      >
        <akd-icon name="search" [size]="14" />
        Apply
      </button>
      @if (filtered()) {
        <button
          class="akd-btn akd-btn--ghost akd-btn--sm"
          type="button"
          [disabled]="loading()"
          (click)="clear()"
        >
          Clear
        </button>
      }
      <span class="grow"></span>
      <button
        class="akd-btn akd-btn--ghost akd-btn--sm"
        type="button"
        [disabled]="loading() || downloading() || events().length === 0"
        (click)="download()"
      >
        <akd-icon name="download" [size]="14" />
        {{ downloading() ? 'Exporting…' : 'Download CSV' }}
      </button>
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <akd-card [padded]="false">
      @if (loading()) {
        <p class="akd-muted pad">Loading…</p>
      } @else if (events().length === 0) {
        <p class="akd-muted pad">No audit events match.</p>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">
            Audit events
          </caption>
          <thead>
            <tr>
              <th scope="col">When</th>
              <th scope="col">Actor</th>
              <th scope="col">Action</th>
              <th scope="col">Target</th>
              <th scope="col">Result</th>
              <th scope="col">IP</th>
            </tr>
          </thead>
          <tbody>
            @for (ev of events(); track ev.uuid) {
              <tr>
                <td class="akd-muted sub-mono">{{ ev.occurred_at | slice: 0 : 19 }}</td>
                <td class="sub-mono">{{ ev.actor_display ?? ev.actor_uuid ?? ev.actor_kind }}</td>
                <td>
                  <span class="akd-badge akd-badge--mono">{{ ev.action }}</span>
                </td>
                <td class="sub-mono target" [title]="ev.target_uuid ?? ''">
                  @if (ev.target_kind) {
                    <span class="akd-muted">{{ ev.target_kind }}</span>
                    <!-- The name as it was WHEN the action happened; older
                         entries have none, and the uuid still identifies the
                         row. -->
                    <span class="target-name">{{ targetLabel(ev) }}</span>
                  } @else {
                    <span class="akd-muted">—</span>
                  }
                </td>
                <td>
                  <span
                    class="akd-badge akd-badge--mono"
                    [class.akd-badge--accent]="ev.result !== 'success'"
                  >
                    {{ ev.result }}
                  </span>
                </td>
                <td class="akd-muted sub-mono">{{ ev.ip ?? '—' }}</td>
              </tr>
            }
          </tbody>
        </table>
      }
    </akd-card>
    <p class="footnote">{{ hint() }}</p>
  `,
  styles: [
    `
      .filters {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--space-2);
        margin-bottom: var(--space-3);
      }
      .filters .akd-input {
        width: auto;
      }
      .grow {
        flex: 1;
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .sub-mono {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
      }
      .target {
        display: flex;
        gap: 6px;
        align-items: baseline;
      }
      .target-name {
        color: var(--text-1);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 22ch;
      }
      .footnote {
        margin: var(--space-3) 0 0;
        font-size: var(--text-xs);
        color: var(--text-3);
      }
    `,
  ],
})
export class AuditLogComponent {
  private readonly api = inject(ApiService);

  /** How to fetch a page (team-scoped or instance-wide). */
  readonly fetch = input.required<AuditFetch>();
  /** Basename of the exported CSV (without extension). */
  readonly exportName = input('audit');

  protected readonly events = signal<AuditEvent[]>([]);
  protected readonly loading = signal(true);
  protected readonly downloading = signal(false);
  protected readonly error = signal<string | null>(null);

  protected fAction = '';
  protected fResult: '' | 'success' | 'failure' | 'denied' = '';
  protected fFrom = '';
  protected fTo = '';

  private started = false;

  constructor() {
    // input() is not readable in a field initializer; defer the first load.
    queueMicrotask(() => {
      if (!this.started) {
        this.started = true;
        void this.load();
      }
    });
  }

  protected filtered(): boolean {
    return !!(this.fAction || this.fResult || this.fFrom || this.fTo);
  }

  /** What to call the target: the name captured with the entry, or the head of
   *  the uuid for the rows written before names were recorded. */
  protected targetLabel(ev: AuditEvent): string {
    if (ev.target_name) return ev.target_name;
    return ev.target_uuid ? ev.target_uuid.slice(0, 8) : '';
  }

  protected hint(): string {
    return `Append-only trail (§23.4). Showing ${this.events().length} event(s) — Download CSV exports every matching event.`;
  }

  private query(cursor?: string): AuditQuery {
    const q: AuditQuery = { limit: 100 };
    if (cursor) q.cursor = cursor;
    if (this.fAction.trim()) q.action = this.fAction.trim();
    if (this.fResult) q.result = this.fResult;
    // A date input is a day; widen `to` to the end of that day.
    if (this.fFrom) q.from = new Date(this.fFrom + 'T00:00:00Z').toISOString();
    if (this.fTo) q.to = new Date(this.fTo + 'T23:59:59Z').toISOString();
    return q;
  }

  protected apply(): void {
    void this.load();
  }

  protected clear(): void {
    this.fAction = '';
    this.fResult = '';
    this.fFrom = '';
    this.fTo = '';
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      const page = await this.fetch()(this.query());
      this.events.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async download(): Promise<void> {
    this.downloading.set(true);
    this.error.set(null);
    try {
      const all: AuditEvent[] = [];
      let cursor: string | undefined;
      // Follow the cursor to export the whole filtered set, with a safety cap so
      // a runaway never freezes the tab.
      for (let page = 0; page < 200; page++) {
        const res = await this.fetch()(this.query(cursor));
        all.push(...res.data);
        if (!res.next_cursor) break;
        cursor = res.next_cursor;
      }
      this.saveCsv(all);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.downloading.set(false);
    }
  }

  private saveCsv(events: AuditEvent[]): void {
    const columns = [
      'occurred_at',
      'actor_kind',
      'actor_display',
      'actor_uuid',
      'action',
      'target_kind',
      'target_name',
      'target_uuid',
      'result',
      'ip',
    ];
    const escape = (v: unknown): string => {
      const s = v == null ? '' : String(v);
      return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
    };
    const rows = events.map((e) =>
      [
        e.occurred_at,
        e.actor_kind,
        e.actor_display,
        e.actor_uuid,
        e.action,
        e.target_kind,
        e.target_name,
        e.target_uuid,
        e.result,
        e.ip,
      ]
        .map(escape)
        .join(','),
    );
    const csv = [columns.join(','), ...rows].join('\n');
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${this.exportName()}-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  }
}
