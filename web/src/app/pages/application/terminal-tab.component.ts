import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { TerminalComponent } from '../../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../../ui/terminal/protocol';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type ServiceComponent = components['schemas']['ServiceComponent'];

/** Shell inside the application's running container (§5.7). */
@Component({
  selector: 'app-application-terminal-tab',
  standalone: true,
  imports: [FormsModule, TerminalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="akd-card">
      <div class="akd-card__header">
        <h2 class="akd-card__title">Terminal</h2>
        @if (components().length > 0) {
          <!-- A compose stack is several containers: pick whose shell
               (compose-spec §2.2) — the stack has no container of its own. -->
          <div class="akd-select">
            <select name="terminalComponent" class="akd-input" [(ngModel)]="component">
              @for (c of components(); track c.name) {
                <option [ngValue]="c.name">{{ c.name }}</option>
              }
            </select>
          </div>
        }
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

  protected readonly components = signal<ServiceComponent[]>([]);
  protected component = '';

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.loadComponents(uuid));
    });
  }

  private async loadComponents(uuid: string): Promise<void> {
    try {
      const page = await this.api.client().listApplicationComponents(uuid);
      this.components.set(page.data);
      if (page.data.length > 0) this.component = page.data[0].name;
    } catch {
      this.components.set([]);
    }
  }

  protected readonly openSession = async (): Promise<TerminalSessionInfo> =>
    (await this.api
      .client()
      .createApplicationTerminalSession(
        this.uuid(),
        this.component ? { component: this.component } : undefined,
      )) as unknown as TerminalSessionInfo;
}
