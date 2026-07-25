import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  input,
  output,
  signal,
  untracked,
} from '@angular/core';
import { CardComponent } from '../card/card.component';
import { IconComponent } from '../icon/icon.component';
import { StatusBadgeComponent } from '../status-badge/status-badge.component';
import type { components } from '../../api/schema';

type ServiceComponent = components['schemas']['ServiceComponent'];
type ComponentMetric = components['schemas']['ComponentMetric'];

/** Where a component action deep-links to, carried up to the host page. */
export interface StackComponentAction {
  target: 'logs' | 'terminal' | 'storages';
  component: string;
}

/** Human-readable byte size (binary units), e.g. 26843546 → "25.6 MiB". */
function fmtBytes(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

/** Default listen port of a database engine, for a ready-to-run port-forward. */
function enginePort(engine: string | null | undefined): number | null {
  switch (engine) {
    case 'postgresql':
      return 5432;
    case 'mysql':
    case 'mariadb':
      return 3306;
    case 'mongodb':
      return 27017;
    case 'redis':
    case 'keydb':
    case 'dragonfly':
      return 6379;
    case 'clickhouse':
      return 9000;
    default:
      return null;
  }
}

/**
 * The stack's compose services as tabs: one panel per service showing its state
 * and the actions that apply to it (logs, shell, volumes) plus the CLI commands
 * to reach it from a laptop. Shared by the application and preview pages — a
 * preview passes `pr` so the generated commands carry `--pr N` (cli.md §7–8).
 */
@Component({
  selector: 'akd-stack-components',
  standalone: true,
  imports: [CardComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <akd-card title="Stack components" [padded]="false">
      <div class="comp">
        <!-- One tab per compose service: its state and the actions that apply to
             it live in its own panel, instead of a flat list to cross-reference. -->
        <nav class="comp__tabs" role="tablist" aria-label="Stack components">
          @for (c of components(); track c.uuid) {
            <button
              type="button"
              role="tab"
              class="comp__tab"
              [class.comp__tab--active]="active() === c.name"
              [attr.aria-selected]="active() === c.name"
              (click)="active.set(c.name)"
            >
              <span class="akd-mono comp__tab-name">{{ c.name }}</span>
              <akd-status-badge domain="resource" [state]="c.observed_status" />
            </button>
          }
        </nav>

        @if (activeComp(); as c) {
          <div class="comp__panel" role="tabpanel">
            <header class="comp__head">
              <span class="akd-mono comp__title">{{ c.name }}</span>
              <akd-status-badge domain="resource" [state]="c.observed_status" />
              @if (c.is_database) {
                <span class="akd-badge akd-badge--mono">db: {{ c.database_engine }}</span>
              }
              @if (c.exclude_from_hc) {
                <span class="akd-badge">one-shot</span>
              }
            </header>

            @if (c.image || c.observed_at) {
              <dl class="comp__meta">
                @if (c.image) {
                  <div>
                    <dt>Image</dt>
                    <dd class="akd-mono">{{ c.image }}</dd>
                  </div>
                }
                @if (c.observed_at) {
                  <div>
                    <dt>Last seen</dt>
                    <dd>{{ c.observed_at }}</dd>
                  </div>
                }
              </dl>
            }

            <!-- Live CPU/RAM, read on demand via docker stats (ADR-034). -->
            @if (hasMetrics()) {
              @if (activeMetric(); as m) {
                <div class="usage">
                  <div class="usage__row">
                    <div class="usage__head">
                      <span class="usage__label">CPU</span>
                      <span class="akd-mono">{{ cpuLabel(m) }}</span>
                    </div>
                    <div class="bar">
                      <div class="bar__fill" [style.width.%]="barWidth(m.cpu_percent)"></div>
                    </div>
                  </div>
                  <div class="usage__row">
                    <div class="usage__head">
                      <span class="usage__label">Memory</span>
                      <span class="akd-mono">{{ memLabel(m) }}</span>
                    </div>
                    <div class="bar">
                      <div class="bar__fill" [style.width.%]="barWidth(m.memory_percent)"></div>
                    </div>
                  </div>
                  @if (sparkPoints(); as pts) {
                    <svg
                      class="spark"
                      viewBox="0 0 100 24"
                      preserveAspectRatio="none"
                      aria-hidden="true"
                    >
                      <polyline [attr.points]="pts" />
                    </svg>
                  }
                </div>
              } @else {
                <p class="usage__empty akd-muted">No live stats — the container is not running.</p>
              }
            }

            <div class="comp__actions">
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                (click)="open.emit({ target: 'logs', component: c.name })"
              >
                <akd-icon name="scroll-text" [size]="13" />
                Logs
              </button>
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                (click)="open.emit({ target: 'terminal', component: c.name })"
              >
                <akd-icon name="terminal" [size]="13" />
                Shell
              </button>
              @if (c.is_database) {
                <button
                  class="akd-btn akd-btn--secondary akd-btn--sm"
                  type="button"
                  (click)="open.emit({ target: 'storages', component: c.name })"
                >
                  <akd-icon name="hard-drive" [size]="13" />
                  Volumes
                </button>
              }
            </div>

            <!-- Reach this container from your machine without exposing it (the
                 CLI tunnels through the manager — cli.md §7). -->
            <div class="comp__cli">
              @if (c.is_database) {
                <span class="comp__cli-label">Open a console</span>
                <div class="comp__cmd">
                  <code class="akd-mono">{{ dbConsoleCmd(c) }}</code>
                  <button
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    type="button"
                    title="Copy"
                    (click)="copy(dbConsoleCmd(c))"
                  >
                    <akd-icon name="copy" [size]="13" />
                  </button>
                </div>
              }
              <span class="comp__cli-label">Forward a port</span>
              <div class="comp__cmd">
                <code class="akd-mono">{{ portForwardCmd(c) }}</code>
                <button
                  class="akd-btn akd-btn--ghost akd-btn--sm"
                  type="button"
                  title="Copy"
                  (click)="copy(portForwardCmd(c))"
                >
                  <akd-icon name="copy" [size]="13" />
                </button>
              </div>
              @if (notice(); as text) {
                <span class="comp__notice">{{ text }}</span>
              }
            </div>
          </div>
        }
      </div>
    </akd-card>
  `,
  styles: [
    `
      /* Left rail of services, right panel for the selected one; stacks on
         narrow screens (the rail becomes a horizontal strip). */
      .comp {
        display: grid;
        grid-template-columns: minmax(11rem, 15rem) 1fr;
        align-items: stretch;
      }
      .comp__tabs {
        display: flex;
        flex-direction: column;
        gap: var(--space-1);
        padding: var(--space-3);
        border-right: 1px solid var(--border-1);
      }
      .comp__tab {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        width: 100%;
        padding: var(--space-2) var(--space-3);
        border: 0;
        border-radius: var(--radius-2);
        background: transparent;
        color: var(--text-1);
        cursor: pointer;
        text-align: left;
        font: inherit;
      }
      .comp__tab:hover {
        background: var(--bg-2);
      }
      .comp__tab:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .comp__tab--active {
        background: var(--bg-2);
      }
      .comp__tab-name {
        flex: 1;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        font-size: var(--text-sm);
      }
      .comp__panel {
        padding: var(--space-4) var(--space-5);
        display: grid;
        gap: var(--space-4);
        align-content: start;
      }
      .comp__head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .comp__title {
        font-size: var(--text-base);
      }
      .comp__meta {
        display: grid;
        gap: var(--space-2);
        margin: 0;
        font-size: var(--text-sm);
      }
      .comp__meta > div {
        display: flex;
        gap: var(--space-3);
      }
      .comp__meta dt {
        min-width: 5.5rem;
        color: var(--text-3);
      }
      .comp__meta dd {
        margin: 0;
        overflow-wrap: anywhere;
      }
      .comp__actions {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-2);
      }
      .usage {
        display: grid;
        gap: var(--space-2);
      }
      .usage__head {
        display: flex;
        justify-content: space-between;
        font-size: var(--text-xs);
      }
      .usage__label {
        color: var(--text-3);
      }
      .bar {
        height: 6px;
        border-radius: var(--radius-1, 3px);
        background: var(--surface-2);
        overflow: hidden;
      }
      .bar__fill {
        height: 100%;
        background: var(--accent);
        transition: width var(--dur-1, 150ms) var(--ease-out, ease);
      }
      .spark {
        width: 100%;
        height: 28px;
        margin-top: var(--space-1);
      }
      .spark polyline {
        fill: none;
        stroke: var(--accent);
        stroke-width: 1.5;
        vector-effect: non-scaling-stroke;
      }
      .usage__empty {
        font-size: var(--text-sm);
        margin: 0;
      }
      .comp__cli {
        display: grid;
        gap: var(--space-2);
        padding-top: var(--space-3);
        border-top: 1px solid var(--border-1);
      }
      .comp__cli-label {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .comp__cmd {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .comp__cmd code {
        flex: 1;
        padding: var(--space-2) var(--space-3);
        border-radius: var(--radius-2);
        background: var(--surface-2);
        font-size: var(--text-xs);
        overflow-x: auto;
        white-space: nowrap;
      }
      .comp__notice {
        font-size: var(--text-xs);
        color: var(--text-success, var(--text-2));
      }
      @media (max-width: 48rem) {
        .comp {
          grid-template-columns: 1fr;
        }
        .comp__tabs {
          flex-direction: row;
          flex-wrap: wrap;
          border-right: 0;
          border-bottom: 1px solid var(--border-1);
        }
        .comp__tab {
          width: auto;
        }
        .comp__tab-name {
          flex: 0 1 auto;
        }
      }
    `,
  ],
})
export class StackComponentsComponent {
  readonly components = input.required<ServiceComponent[]>();
  /** CLI reference base: commands target `app/<appName>` (name or UUID). */
  readonly appName = input<string>('');
  /** When set, the target is a PR preview: commands carry `--pr N`. */
  readonly pr = input<number | undefined>(undefined);
  /** Live per-service stats, keyed by component name (ADR-034). The host page
   * polls and feeds fresh snapshots; empty = the feature is not wired here. */
  readonly metrics = input<Record<string, ComponentMetric>>({});
  readonly open = output<StackComponentAction>();

  /** Which service's panel is open; kept valid as the list changes. */
  protected readonly active = signal<string | null>(null);
  protected readonly activeComp = computed(
    () => this.components().find((c) => c.name === this.active()) ?? null,
  );

  /** True once the host feeds any snapshot — gates the whole usage block. */
  protected readonly hasMetrics = computed(() => Object.keys(this.metrics()).length > 0);
  protected readonly activeMetric = computed<ComponentMetric | null>(
    () => this.metrics()[this.active() ?? ''] ?? null,
  );

  /** A short CPU history per service, appended on each snapshot, for a live
   * sparkline (kept in memory only — nothing is stored, ADR-034). */
  private readonly SPARK_LEN = 40;
  private readonly cpuHistory = signal<Map<string, number[]>>(new Map());
  /** SVG polyline points for the active service's CPU trend, or '' if too few. */
  protected readonly sparkPoints = computed(() => {
    const series = this.cpuHistory().get(this.active() ?? '') ?? [];
    if (series.length < 2) return '';
    const max = Math.max(...series, 1);
    const n = series.length;
    return series
      .map((v, i) => `${((i / (n - 1)) * 100).toFixed(1)},${(24 - (v / max) * 24).toFixed(1)}`)
      .join(' ');
  });

  protected readonly notice = signal<string | null>(null);
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    effect(() => {
      const comps = this.components();
      const current = untracked(() => this.active());
      if (!current || !comps.some((c) => c.name === current)) {
        this.active.set(comps[0]?.name ?? null);
      }
    });
    // Each new snapshot appends every service's CPU to its ring buffer.
    effect(() => {
      const snap = this.metrics();
      untracked(() => {
        const next = new Map(this.cpuHistory());
        for (const [name, m] of Object.entries(snap)) {
          if (m.cpu_percent == null) continue;
          const series = [...(next.get(name) ?? []), m.cpu_percent];
          if (series.length > this.SPARK_LEN) series.splice(0, series.length - this.SPARK_LEN);
          next.set(name, series);
        }
        this.cpuHistory.set(next);
      });
    });
    inject(DestroyRef).onDestroy(() => {
      if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    });
  }

  /** Bar fill for a percentage that can exceed 100 (multi-core CPU). */
  protected barWidth(pct: number | null | undefined): number {
    if (pct == null) return 0;
    return Math.max(0, Math.min(100, pct));
  }

  protected cpuLabel(m: ComponentMetric): string {
    return m.cpu_percent == null ? '—' : `${m.cpu_percent.toFixed(1)}%`;
  }

  protected memLabel(m: ComponentMetric): string {
    if (m.memory_bytes == null) return '—';
    const used = fmtBytes(m.memory_bytes);
    const pct = m.memory_percent == null ? '' : ` · ${m.memory_percent.toFixed(0)}%`;
    return m.memory_limit_bytes ? `${used} / ${fmtBytes(m.memory_limit_bytes)}${pct}` : used;
  }

  private appRef(): string {
    return 'app/' + this.appName();
  }

  private prSuffix(): string {
    const pr = this.pr();
    return pr ? ` --pr ${pr}` : '';
  }

  /** Confort console for a database service (cli.md §8). */
  protected dbConsoleCmd(c: ServiceComponent): string {
    return `akerdock db ${this.appRef()} -c ${c.name}${this.prSuffix()}`;
  }

  /** TCP tunnel through the manager to this service (cli.md §7). */
  protected portForwardCmd(c: ServiceComponent): string {
    const port = enginePort(c.database_engine) ?? '<PORT>';
    return `akerdock port-forward ${this.appRef()} ${port}:${port} -c ${c.name}${this.prSuffix()}`;
  }

  protected async copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      this.flashNotice('Copied to clipboard.');
    } catch {
      this.flashNotice('Copy failed — select and copy manually.');
    }
  }

  private flashNotice(text: string): void {
    this.notice.set(text);
    if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => this.notice.set(null), 2500);
  }
}
