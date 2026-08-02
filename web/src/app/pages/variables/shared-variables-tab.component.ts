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
import { CardComponent } from '../../../ui/card/card.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { fetchAll } from '../../core/pagination';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import {
  sharedVariableCreatePayload,
  sharedVariableEditValue,
  sharedVariableUpdatePayload,
  sharedVariablesOf,
  type ScopedSharedVariableScope,
} from '../../core/shared-variables';
import type { components } from '../../../api/schema';

type SharedVariable = components['schemas']['SharedVariable'];

/**
 * The shared variables of ONE parent — a project, an environment or a server
 * (§5.4). The same table serves the three scopes: they differ only by the
 * reference they render (`{{project.KEY}}`…) and by the parent the API hangs
 * them off, so the page that owns the parent just names its scope.
 *
 * The team scope stays on the team settings page: it has no parent, and its
 * table also carries the whole-team inventory.
 */
@Component({
  selector: 'akd-shared-variables',
  standalone: true,
  imports: [FormsModule, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else {
      <akd-card [title]="heading()" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Shared variables of this
            {{
              scope()
            }}
          </caption>
          <thead>
            <tr>
              <th scope="col">Key</th>
              <th scope="col">Value</th>
              <th scope="col">Flags</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (v of variables(); track v.uuid) {
              <tr>
                <td>
                  <span class="akd-mono">{{ v.key }}</span>
                  <div class="ref akd-mono akd-muted">
                    {{ '{{' }}{{ scope() }}.{{ v.key }}{{ '}}' }}
                  </div>
                </td>
                @if (editing() === v.uuid) {
                  <!-- The key and the scope are identity: only the value and
                       the masking are editable (recreate to rename). -->
                  <td>
                    <input
                      class="akd-input akd-input--mono"
                      name="editValue"
                      [attr.aria-label]="'Value of ' + v.key"
                      [placeholder]="v.is_redacted ? '•••••• unchanged' : 'value'"
                      [(ngModel)]="editValue"
                      [disabled]="busy()"
                      (keydown.enter)="save(v)"
                      (keydown.escape)="cancelEdit()"
                    />
                    @if (v.is_redacted) {
                      <div class="ref akd-muted">Leave empty to keep the stored value.</div>
                    }
                  </td>
                  <td>
                    <label class="akd-check">
                      <input
                        type="checkbox"
                        name="editSecret"
                        [(ngModel)]="editSecret"
                        [disabled]="busy()"
                      />
                      secret
                    </label>
                  </td>
                  <td class="right">
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="save(v)"
                    >
                      Save
                    </button>
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="cancelEdit()"
                    >
                      Cancel
                    </button>
                  </td>
                } @else {
                  <td class="akd-mono akd-muted">{{ v.is_redacted ? '••••••••' : v.value }}</td>
                  <td>
                    @if (v.is_secret) {
                      <span class="akd-badge akd-badge--accent">secret</span>
                    } @else {
                      <span class="akd-muted">—</span>
                    }
                  </td>
                  <td class="right">
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="startEdit(v)"
                    >
                      Edit
                    </button>
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(v)"
                    >
                      Delete
                    </button>
                  </td>
                }
              </tr>
            }
            <!-- The last row IS the creator: add a variable in place. -->
            <tr class="add-row">
              <td>
                <input
                  class="akd-input akd-input--mono"
                  name="newKey"
                  placeholder="NEW_KEY"
                  aria-label="New variable key"
                  [(ngModel)]="newKey"
                  [disabled]="busy()"
                  (keydown.enter)="create()"
                />
              </td>
              <td>
                <input
                  class="akd-input akd-input--mono"
                  name="newValue"
                  placeholder="value"
                  aria-label="New variable value"
                  [(ngModel)]="newValue"
                  [disabled]="busy()"
                  (keydown.enter)="create()"
                />
              </td>
              <td>
                <label class="akd-check" title="Encrypted at rest, never shown again (INV-003)">
                  <input
                    type="checkbox"
                    name="newSecret"
                    [(ngModel)]="newSecret"
                    [disabled]="busy()"
                  />
                  secret
                </label>
              </td>
              <td class="right">
                <button
                  class="akd-btn akd-btn--primary akd-btn--sm"
                  type="button"
                  [disabled]="busy() || !newKey.trim()"
                  (click)="create()"
                >
                  <akd-icon name="plus" [size]="13" />
                  Add
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </akd-card>

      <p class="footnote">
        Reference these in any resource {{ reach() }} as
        <code class="akd-mono">{{ '{{' }}{{ scope() }}.KEY{{ '}}' }}</code>. Interpolated at deploy
        time; a resource's own variable wins, and an unknown reference stays verbatim in the
        container (visible, therefore diagnosable). Previews never receive shared secrets.
      </p>
    }
  `,
  styles: [
    `
      .ref {
        font-size: var(--text-xs);
        margin-top: 2px;
      }
      .footnote {
        margin-top: var(--space-3);
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .footnote code {
        color: var(--text-2);
      }
      .add-row td {
        vertical-align: middle;
      }
      .add-row .akd-input {
        width: 100%;
      }
    `,
  ],
})
export class SharedVariablesComponent {
  /** Which inheritance level this table owns, and the entity it hangs off. */
  readonly scope = input.required<ScopedSharedVariableScope>();
  readonly parentUuid = input.required<string>();
  /** Card title — defaults to the scope, e.g. "Project variables". */
  readonly heading = input<string>('Shared variables');
  /** How the footnote names what these variables reach, e.g. "in this project". */
  readonly reach = input<string>('below this level');

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly variables = signal<SharedVariable[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected newKey = '';
  protected newValue = '';
  protected newSecret = false;
  /** UUID of the row open for editing — one at a time. */
  protected readonly editing = signal<string | null>(null);
  protected editValue = '';
  protected editSecret = false;

  constructor() {
    effect(() => {
      const scope = this.scope();
      const parent = this.parentUuid();
      untracked(() => void this.load(scope, parent));
    });
  }

  private async load(scope: ScopedSharedVariableScope, parentUuid: string): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      await this.reload(scope, parentUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private async reload(
    scope: ScopedSharedVariableScope = this.scope(),
    parentUuid: string = this.parentUuid(),
  ): Promise<void> {
    const page = await fetchAll((cursor) =>
      this.api.client().listSharedVariables({ scope, limit: 100, cursor }),
    );
    this.variables.set(sharedVariablesOf(page, scope, parentUuid));
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.newKey.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createSharedVariable(
        sharedVariableCreatePayload(this.scope(), this.parentUuid(), {
          key: this.newKey,
          value: this.newValue,
          secret: this.newSecret,
        }),
      );
      this.newKey = '';
      this.newValue = '';
      this.newSecret = false;
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected startEdit(v: SharedVariable): void {
    this.editing.set(v.uuid);
    this.editValue = sharedVariableEditValue(v);
    this.editSecret = v.is_secret;
    this.error.set(null);
  }

  protected cancelEdit(): void {
    this.editing.set(null);
  }

  /** Saves the edited row. The value only reaches the API when it changed —
   * a redacted variable left untouched keeps the value nobody could read. */
  protected async save(v: SharedVariable): Promise<void> {
    if (this.busy()) return;
    const body = sharedVariableUpdatePayload(v, { value: this.editValue, secret: this.editSecret });
    if (!body) {
      this.editing.set(null);
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateSharedVariable(v.uuid, body);
      this.editing.set(null);
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(v: SharedVariable): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the variable',
        message: `Delete the ${this.scope()} variable "${v.key}"? Resources pick it up at their next deployment.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteSharedVariable(v.uuid);
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
