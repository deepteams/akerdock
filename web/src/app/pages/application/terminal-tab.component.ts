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
      <akd-terminal
        title="Application shell"
        hint="Opens a shell in the running container. The container must be running."
        [open]="openSession"
      />
    </section>
  `,
})
export class ApplicationTerminalTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly openSession = async (): Promise<TerminalSessionInfo> =>
    (await this.api
      .client()
      .createApplicationTerminalSession(this.uuid())) as unknown as TerminalSessionInfo;
}
