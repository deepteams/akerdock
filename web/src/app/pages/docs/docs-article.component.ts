import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { RouterLink } from '@angular/router';
import { IconComponent } from '../../../ui/icon/icon.component';
import { isBeyondRole, sectionAnchor, type DocTopic } from './docs.model';

/**
 * Renders one chapter of the manual: its sections, in order, each as the HTML
 * the server produced from the Markdown source (ADR-072 §3).
 *
 * The `html` is bound with `[innerHTML]`, which runs Angular's DomSanitizer.
 * That is the second half of the safety argument, the first being that the
 * markup was produced by goldmark itself with raw HTML disabled — a tag
 * written inside a `.md` is dropped by the parser, never forwarded. Nothing
 * here may reach for DomSanitizer's trust-me escape hatches: the day it does,
 * the sanitiser stops being a control and the whole argument is one call deep.
 * A spec asserts their absence — by name, which is why the name is not spelled
 * out here — and another asserts that hostile markup bound in this template
 * really does come out inert.
 *
 * Typography lives in `.akd-prose` (styles.css) rather than in this
 * component's styles: emulated encapsulation stamps its attribute on the
 * template's own elements, and content inserted through `[innerHTML]` never
 * carries it — a scoped rule would simply not match the manual's paragraphs.
 */
@Component({
  selector: 'app-docs-article',
  standalone: true,
  imports: [RouterLink, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <article class="article">
      <header class="head">
        <a class="back akd-btn akd-btn--ghost akd-btn--sm" routerLink="/docs">
          <akd-icon name="arrow-left" [size]="13" />
          All pages
        </a>
        <h1>
          <akd-icon [name]="topic().icon || 'book-open'" [size]="19" />
          {{ topic().title }}
        </h1>
        <p class="summary">{{ topic().summary }}</p>

        @if (beyond()) {
          <p class="beyond" role="note">
            <akd-icon name="lock" [size]="13" />
            <span>
              Your role does not grant this — the page is shown because you asked to see the whole
              manual. A team administrator can grant it.
            </span>
          </p>
        }

        @if (topic().links; as links) {
          <nav class="links" aria-label="Where to do this">
            @for (link of links; track link.label) {
              @if (link.route) {
                <a class="akd-btn akd-btn--secondary akd-btn--sm" [routerLink]="link.route">
                  {{ link.label }}
                  <akd-icon name="chevron-right" [size]="13" />
                </a>
              } @else if (link.href) {
                <a
                  class="akd-btn akd-btn--secondary akd-btn--sm"
                  [href]="link.href"
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {{ link.label }}
                  <akd-icon name="external-link" [size]="13" />
                </a>
              }
            }
          </nav>
        }
      </header>

      @if (topic().intro_html; as intro) {
        <div class="akd-prose intro" [innerHTML]="intro"></div>
      }

      @if (topic().sections.length > 1) {
        <nav class="toc" aria-label="On this page">
          @for (section of topic().sections; track section.id) {
            <a [href]="'#' + anchor(section.id)">{{ section.title }}</a>
          }
        </nav>
      }

      @for (section of topic().sections; track section.id) {
        <section class="section" [id]="anchor(section.id)">
          <h2>
            {{ section.title }}
            @if (sectionBeyond(section.beyond_role)) {
              <span class="akd-badge">beyond your role</span>
            }
          </h2>
          <div class="akd-prose" [innerHTML]="section.html"></div>
        </section>
      } @empty {
        <p class="akd-muted">This page has no content yet.</p>
      }
    </article>
  `,
  styles: [
    `
      .article {
        max-width: 78ch;
      }
      .head h1 {
        display: flex;
        align-items: center;
        gap: 10px;
        margin: 0;
        font: var(--weight-bold) var(--text-2xl) var(--font-display);
        color: var(--text-1);
      }
      .back {
        margin-bottom: var(--space-3);
        text-decoration: none;
      }
      .summary {
        margin: 6px 0 0;
        color: var(--text-3);
        font-size: var(--text-md);
      }
      .beyond {
        display: flex;
        align-items: flex-start;
        gap: 8px;
        margin: var(--space-3) 0 0;
        padding: var(--space-2) var(--space-3);
        border: 1px solid var(--warn-border);
        background: var(--warn-dim);
        border-radius: var(--radius-2);
        color: var(--text-2);
        font-size: var(--text-sm);
      }
      .beyond akd-icon {
        color: var(--warn);
      }
      .links {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-2);
        margin-top: var(--space-4);
      }
      .links a {
        text-decoration: none;
      }

      .intro {
        margin-top: var(--space-5);
      }

      .toc {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-3);
        margin: var(--space-5) 0 0;
        padding: var(--space-3);
        background: var(--bg-1);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-3);
        font-size: var(--text-sm);
      }
      .toc a {
        color: var(--text-3);
      }
      .toc a:hover {
        color: var(--accent);
      }

      .section {
        margin-top: var(--space-6);
        scroll-margin-top: var(--space-4);
      }
      .section h2 {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        margin: 0 0 var(--space-3);
        font: var(--weight-semibold) var(--text-lg) var(--font-display);
        color: var(--text-1);
      }
    `,
  ],
})
export class DocsArticleComponent {
  readonly topic = input.required<DocTopic>();

  /** The chapter is only in the response because "show everything" is on. */
  protected readonly beyond = computed(() => isBeyondRole(this.topic()));

  protected anchor(sectionId: string): string {
    return sectionAnchor(this.topic().id, sectionId);
  }

  protected sectionBeyond(flag: boolean | undefined): boolean {
    return flag === true;
  }
}
