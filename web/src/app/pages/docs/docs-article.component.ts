import { ChangeDetectionStrategy, Component, input, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { IconComponent } from '../../../ui/icon/icon.component';
import { DocTextComponent } from './doc-text.component';
import type { DocTopic } from './docs.model';

/**
 * Renders one documentation topic: its blocks, in order, each bound as text.
 *
 * The content file holds data, not markup, and this renderer is the reason it
 * can stay that way — every string goes through <app-doc-text>, which parses
 * the inline vocabulary into runs. Nothing here writes HTML.
 */
@Component({
  selector: 'app-docs-article',
  standalone: true,
  imports: [RouterLink, IconComponent, DocTextComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <article class="article">
      <header class="head">
        <a class="back akd-btn akd-btn--ghost akd-btn--sm" routerLink="/docs">
          <akd-icon name="arrow-left" [size]="13" />
          All pages
        </a>
        <h1>
          <akd-icon [name]="topic().icon" [size]="19" />
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

      @if (topic().sections.length > 1) {
        <nav class="toc" aria-label="On this page">
          @for (section of topic().sections; track section.id) {
            <a [href]="'#' + anchor(section.id)">{{ section.title }}</a>
          }
        </nav>
      }

      @for (section of topic().sections; track section.id) {
        <section class="section" [id]="anchor(section.id)">
          <h2>{{ section.title }}</h2>

          @for (block of section.blocks; track $index) {
            @switch (block.kind) {
              @case ('p') {
                <p class="txt"><app-doc-text [text]="block.text" /></p>
              }
              @case ('note') {
                <aside class="callout note">
                  <akd-icon name="info" [size]="14" />
                  <p><app-doc-text [text]="block.text" /></p>
                </aside>
              }
              @case ('warn') {
                <aside class="callout warn">
                  <akd-icon name="triangle-alert" [size]="14" />
                  <p><app-doc-text [text]="block.text" /></p>
                </aside>
              }
              @case ('ul') {
                <ul class="list">
                  @for (item of block.items; track $index) {
                    <li><app-doc-text [text]="item" /></li>
                  }
                </ul>
              }
              @case ('steps') {
                <ol class="list steps">
                  @for (item of block.items; track $index) {
                    <li><app-doc-text [text]="item" /></li>
                  }
                </ol>
              }
              @case ('code') {
                <figure class="snippet">
                  @if (block.caption) {
                    <figcaption>{{ block.caption }}</figcaption>
                  }
                  <button
                    type="button"
                    class="akd-iconbtn copy"
                    aria-label="Copy this snippet"
                    (click)="copy(block.code)"
                  >
                    <akd-icon [name]="copied() === block.code ? 'check' : 'copy'" [size]="13" />
                  </button>
                  <pre><code>{{ block.code }}</code></pre>
                </figure>
              }
              @case ('table') {
                <div class="table-wrap">
                  <table class="akd-table">
                    <thead>
                      <tr>
                        @for (cell of block.head; track $index) {
                          <th scope="col">{{ cell }}</th>
                        }
                      </tr>
                    </thead>
                    <tbody>
                      @for (row of block.rows; track $index) {
                        <tr>
                          @for (cell of row; track $index) {
                            <td><app-doc-text [text]="cell" /></td>
                          }
                        </tr>
                      }
                    </tbody>
                  </table>
                </div>
              }
            }
          }
        </section>
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
        margin: 0 0 var(--space-3);
        font: var(--weight-semibold) var(--text-lg) var(--font-display);
        color: var(--text-1);
      }
      .txt,
      .list li {
        color: var(--text-2);
        font-size: var(--text-sm);
        line-height: 1.65;
      }
      .txt {
        margin: 0 0 var(--space-3);
      }
      .list {
        margin: 0 0 var(--space-3);
        padding-left: var(--space-5);
      }
      .list li {
        margin-bottom: 6px;
      }
      .steps li::marker {
        color: var(--accent);
        font-family: var(--font-mono);
      }

      .callout {
        display: flex;
        align-items: flex-start;
        gap: 9px;
        margin: 0 0 var(--space-3);
        padding: var(--space-3);
        border: 1px solid var(--border-1);
        border-left-width: 3px;
        border-radius: var(--radius-2);
        background: var(--bg-1);
      }
      .callout p {
        margin: 0;
        font-size: var(--text-sm);
        line-height: 1.6;
        color: var(--text-2);
      }
      .callout.note {
        border-left-color: var(--accent);
      }
      .callout.note akd-icon {
        color: var(--accent);
      }
      .callout.warn {
        border-left-color: var(--warn);
      }
      .callout.warn akd-icon {
        color: var(--warn);
      }

      .snippet {
        position: relative;
        margin: 0 0 var(--space-4);
      }
      .snippet figcaption {
        margin-bottom: 6px;
        font-size: var(--text-2xs);
        color: var(--text-3);
      }
      .snippet pre {
        margin: 0;
        padding: var(--space-3);
        overflow-x: auto;
        background: var(--bg-0);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-2);
      }
      .snippet pre code {
        font-family: var(--font-mono);
        font-size: var(--text-sm);
        line-height: 1.6;
        color: var(--text-2);
      }
      .copy {
        position: absolute;
        top: var(--space-1);
        right: var(--space-1);
        opacity: 0.6;
      }
      .copy:hover {
        opacity: 1;
      }

      .table-wrap {
        overflow-x: auto;
        margin: 0 0 var(--space-4);
      }
      .table-wrap td {
        font-size: var(--text-sm);
        line-height: 1.55;
        vertical-align: top;
      }
    `,
  ],
})
export class DocsArticleComponent {
  readonly topic = input.required<DocTopic>();
  /** The topic is only visible because "show everything" is on. */
  readonly beyond = input<boolean>(false);

  protected readonly copied = signal<string | null>(null);

  /** Anchors are namespaced: a section id is unique inside its topic only. */
  protected anchor(sectionId: string): string {
    return `${this.topic().id}--${sectionId}`;
  }

  protected async copy(code: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(code);
      this.copied.set(code);
      setTimeout(() => this.copied.set(null), 1500);
    } catch {
      /* a browser that refuses the clipboard leaves the snippet selectable */
    }
  }
}
