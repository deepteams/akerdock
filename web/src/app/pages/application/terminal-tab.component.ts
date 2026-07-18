import { ChangeDetectionStrategy, Component, inject, input } from '@angular/core';
import { TerminalComponent } from '../../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../../ui/terminal/protocol';
import { ApiService } from '../../core/api.service';

/** Shell inside the application's running container (§5.7). */
@Component({
  selector: 'app-application-terminal-tab',
  standalone: true,
  imports: [TerminalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="akd-card">
      <div class="akd-card__header">
        <h2 class="akd-card__title">Terminal</h2>
        <span class="spacer"></span>
        <!-- The connection state, controls and timeouts live inside akd-terminal;
             this banner only carries the audit contract (§5.7, §24.4). -->
        <span class="note">opening and closing are audited · keystrokes are never logged</span>
      </div>
      <div class="akd-card__body">
        <akd-terminal
          title="Application shell"
          hint="Opens a shell in the running container. The container must be running."
          [open]="openSession"
        />
      </div>
    </section>
  `,
  styles: [
    `
      .spacer {
        flex: 1;
      }
      .note {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
    `,
  ],
})
export class ApplicationTerminalTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly openSession = async (): Promise<TerminalSessionInfo> =>
    (await this.api
      .client()
      .createApplicationTerminalSession(this.uuid())) as unknown as TerminalSessionInfo;
}
