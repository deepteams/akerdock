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

/** Where a component action deep-links to, carried up to the host page. */
export interface StackComponentAction {
  target: 'logs' | 'terminal' | 'storages';
  component: string;
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
  readonly open = output<StackComponentAction>();

  /** Which service's panel is open; kept valid as the list changes. */
  protected readonly active = signal<string | null>(null);
  protected readonly activeComp = computed(
    () => this.components().find((c) => c.name === this.active()) ?? null,
  );

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
    inject(DestroyRef).onDestroy(() => {
      if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    });
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
