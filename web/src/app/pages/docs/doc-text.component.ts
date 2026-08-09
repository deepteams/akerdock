import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { inlineRuns } from './docs.model';

/**
 * One string of documentation text, rendered as bound runs.
 *
 * It exists so no template in the manual ever touches innerHTML: `code` and
 * **strong** arrive as parsed runs and leave as text nodes. Every place that
 * prints documentation text — paragraph, list item, callout, table cell —
 * goes through here, which is also what keeps the markup vocabulary from
 * quietly growing in one renderer and not the others.
 */
@Component({
  selector: 'app-doc-text',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  // Written without a line break inside a branch on purpose: any whitespace
  // around the interpolation is a text node, and a text node between a `code`
  // span and the comma that follows it renders as "`x` ," in the sentence.
  // prettier-ignore
  template: `@for (run of runs(); track $index) {@if (run.code) {<code>{{ run.text }}</code>} @else if (run.strong) {<strong>{{ run.text }}</strong>} @else {<span>{{ run.text }}</span>}}`,
  styles: [
    `
      :host {
        display: inline;
      }
      code {
        font-family: var(--font-mono);
        font-size: 0.92em;
        color: var(--text-1);
        background: var(--bg-2);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-1);
        padding: 0 4px;
      }
      strong {
        color: var(--text-1);
        font-weight: var(--weight-semibold);
      }
    `,
  ],
})
export class DocTextComponent {
  readonly text = input.required<string>();
  protected readonly runs = computed(() => inlineRuns(this.text()));
}
