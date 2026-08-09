import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../../core/api.service';
import { IconComponent } from '../../../ui/icon/icon.component';
import { DOC_TOPICS } from './docs.content';
import { DocsArticleComponent } from './docs-article.component';
import { groupTopics, narrowTopics, searchTopics, type DocTopic } from './docs.model';

/**
 * The in-app manual: one page naming everything the platform can do, filtered
 * to what THIS session may actually do.
 *
 * The filtering is the point, not a detail. The reader is a developer in the
 * middle of shipping something, and a manual that documents the buttons their
 * role does not have is a manual they have to mentally diff against their own
 * dashboard. So `can()` — the same predicate the sidebar uses — prunes topics,
 * sections and individual blocks. The escape hatch is a switch: someone who
 * wants to know what they are missing (to ask for it) can see the whole thing,
 * marked as beyond their permissions rather than silently mixed in.
 */
@Component({
  selector: 'app-docs',
  standalone: true,
  imports: [FormsModule, RouterLink, IconComponent, DocsArticleComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page docs">
      <aside class="rail">
        <header class="rail-head">
          <label class="search">
            <akd-icon name="search" [size]="14" />
            <input
              class="akd-input"
              type="search"
              placeholder="Search the manual"
              aria-label="Search the documentation"
              [ngModel]="query()"
              (ngModelChange)="query.set($event)"
            />
          </label>
        </header>

        <nav class="rail-nav" aria-label="Documentation">
          @for (group of groups(); track group.title) {
            <span class="akd-sidenav__section">{{ group.title }}</span>
            @for (t of group.topics; track t.id) {
              <a
                class="akd-sidenav__item"
                [routerLink]="['/docs', t.id]"
                [class.akd-sidenav__item--active]="t.id === current()?.id"
                [title]="t.title"
              >
                <akd-icon [name]="t.icon" [size]="14" />
                <span class="rail-label">{{ t.title }}</span>
                @if (beyond(t)) {
                  <akd-icon name="lock" [size]="12" />
                }
              </a>
            }
          } @empty {
            <p class="akd-muted sm none">Nothing matches “{{ query() }}”.</p>
          }
        </nav>

        <footer class="rail-foot">
          <label class="akd-check">
            <input type="checkbox" [checked]="showAll()" (change)="toggleShowAll()" />
            <span>Include what my role cannot do</span>
          </label>
        </footer>
      </aside>

      <div class="body">
        @if (current(); as topic) {
          <app-docs-article [topic]="topic" [beyond]="beyond(topic)" />
        } @else if (topicId()) {
          <div class="akd-empty">
            <akd-icon class="akd-empty__icon" name="square-dashed" [size]="22" />
            <p class="akd-empty__title">No such page</p>
            <p class="akd-empty__msg">
              This page does not exist, or your role does not open onto it. Pick one from the list.
            </p>
          </div>
        } @else {
          <header class="akd-bar">
            <div>
              <h1>Documentation</h1>
              <p class="lede akd-muted">
                Everything this dashboard can do, as your role can do it. {{ counted() }}
              </p>
            </div>
          </header>

          @for (group of groups(); track group.title) {
            <section class="group">
              <h2>{{ group.title }}</h2>
              <div class="cards">
                @for (t of group.topics; track t.id) {
                  <a class="card" [routerLink]="['/docs', t.id]">
                    <span class="card-head">
                      <akd-icon [name]="t.icon" [size]="15" />
                      <span class="card-title">{{ t.title }}</span>
                      @if (beyond(t)) {
                        <span class="akd-badge">beyond your role</span>
                      }
                    </span>
                    <span class="card-summary">{{ t.summary }}</span>
                  </a>
                }
              </div>
            </section>
          } @empty {
            <div class="akd-empty">
              <akd-icon class="akd-empty__icon" name="search" [size]="22" />
              <p class="akd-empty__title">Nothing matches “{{ query() }}”</p>
              <p class="akd-empty__msg">Try a shorter term, or clear the search.</p>
            </div>
          }
        }
      </div>
    </div>
  `,
  styles: [
    `
      .docs {
        display: grid;
        grid-template-columns: 250px minmax(0, 1fr);
        gap: var(--space-6);
        align-items: start;
      }
      @media (max-width: 900px) {
        .docs {
          grid-template-columns: minmax(0, 1fr);
        }
        .rail {
          position: static;
          max-height: none;
        }
      }
      .rail {
        position: sticky;
        top: 0;
        display: flex;
        flex-direction: column;
        gap: var(--space-2);
        max-height: calc(100vh - 52px - var(--space-6) * 2);
        min-height: 0;
      }
      .rail-nav {
        overflow-y: auto;
        min-height: 0;
        flex: 1;
      }
      .search {
        display: flex;
        align-items: center;
        gap: 6px;
        color: var(--text-3);
      }
      .search input {
        width: 100%;
      }
      .rail-label {
        flex: 1;
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .akd-sidenav__item {
        text-decoration: none;
      }
      .akd-sidenav__item:hover {
        text-decoration: none;
      }
      .rail-foot {
        border-top: 1px solid var(--border-1);
        padding-top: var(--space-2);
        font-size: var(--text-2xs);
        color: var(--text-3);
      }
      .none {
        padding: var(--space-3);
      }

      .lede {
        margin: 4px 0 0;
        font-size: var(--text-sm);
        max-width: 62ch;
      }
      .group {
        margin-bottom: var(--space-6);
      }
      .group h2 {
        margin: 0 0 var(--space-3);
        font: var(--weight-semibold) var(--text-md) var(--font-display);
        color: var(--text-2);
      }
      .cards {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
        gap: var(--space-3);
      }
      .card {
        display: flex;
        flex-direction: column;
        gap: 6px;
        padding: var(--space-4);
        background: var(--bg-1);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-3);
        color: var(--text-1);
        text-decoration: none;
        transition: border-color var(--dur-1) var(--ease-out);
      }
      .card:hover {
        border-color: var(--accent-border);
        text-decoration: none;
      }
      .card:focus-visible {
        box-shadow: var(--ring-focus);
      }
      .card-head {
        display: flex;
        align-items: center;
        gap: 8px;
      }
      .card-title {
        font: var(--weight-semibold) var(--text-sm) var(--font-display);
      }
      .card-summary {
        font-size: var(--text-sm);
        color: var(--text-3);
        line-height: 1.5;
      }
    `,
  ],
})
export class DocsComponent {
  private readonly api = inject(ApiService);

  /** Bound from the `:topic` route parameter (withComponentInputBinding). */
  readonly topic = input<string>('');

  protected readonly query = signal('');
  /** Persisted: someone who turned the full manual on meant it. */
  protected readonly showAll = signal(localStorage.getItem('akd.docs.all') === '1');

  protected readonly topicId = computed(() => this.topic());

  private readonly can = (permission: string) => this.api.can(permission);
  private readonly isRoot = computed(() => this.api.currentUser()?.instanceRoot === true);

  /** The manual as this session may read it — the whole point of the page. */
  private readonly visible = computed(() =>
    narrowTopics(DOC_TOPICS, this.can, this.isRoot(), this.showAll()),
  );

  /** Search applies to the navigation, never to the article being read: a
   *  filter that could yank the page out from under the reader is a trap. */
  protected readonly groups = computed(() =>
    groupTopics(searchTopics(this.visible(), this.query())),
  );

  protected readonly current = computed(() => {
    const id = this.topicId();
    if (!id) return null;
    return this.visible().find((t) => t.id === id) ?? null;
  });

  /** True when the topic only shows because "include what I cannot do" is on. */
  protected beyond(topic: DocTopic): boolean {
    if (!this.showAll()) return false;
    return narrowTopics([topic], this.can, this.isRoot(), false).length === 0;
  }

  protected counted(): string {
    const total = this.visible().length;
    return total === 1 ? '1 page.' : `${total} pages.`;
  }

  protected toggleShowAll(): void {
    this.showAll.update((on) => !on);
    localStorage.setItem('akd.docs.all', this.showAll() ? '1' : '0');
  }
}
