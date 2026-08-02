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
import { resourceEnvEditValue, resourceEnvUpdatePayload } from '../../core/resource-envs';
import type { components } from '../../../api/schema';

type EnvVar = components['schemas']['EnvironmentVariable'];

/**
 * The environment variables of ONE compose stack (compose-spec §3.2). The
 * stack's own set: what `${VAR}` in the compose file resolves against, on top
 * of the shared variables inherited from the team, project, environment and
 * server.
 *
 * A locked variable (§5.4) is masked for everyone and not re-editable —
 * delete and recreate is the only path, so its Edit button is not offered.
 */
@Component({
  selector: 'akd-service-envs',
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
      <akd-card title="Environment variables" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Environment variables of this stack
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
            @for (env of envs(); track env.uuid) {
              <tr>
                <td class="akd-mono">{{ env.key }}</td>
                @if (editing() === env.uuid) {
                  <td>
                    <input
                      class="akd-input akd-input--mono"
                      name="editValue"
                      [attr.aria-label]="'Value of ' + env.key"
                      [placeholder]="env.is_redacted ? '•••••• unchanged' : 'value'"
                      [(ngModel)]="editValue"
                      [disabled]="busy()"
                      (keydown.enter)="save(env)"
                      (keydown.escape)="editing.set(null)"
                    />
                    @if (env.is_redacted) {
                      <div class="hint akd-muted">Leave empty to keep the stored value.</div>
                    }
                  </td>
                  <td>
                    <label class="akd-check" title="Value used as-is, without interpolation">
                      <input
                        type="checkbox"
                        name="editLiteral"
                        [(ngModel)]="editLiteral"
                        [disabled]="busy()"
                      />
                      literal
                    </label>
                  </td>
                  <td class="right">
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="save(env)"
                    >
                      Save
                    </button>
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="editing.set(null)"
                    >
                      Cancel
                    </button>
                  </td>
                } @else {
                  <td class="akd-mono akd-muted">
                    {{ env.is_redacted ? '••••••••' : env.value }}
                  </td>
                  <td>
                    @if (env.is_secret) {
                      <span class="akd-badge akd-badge--accent">secret</span>
                    }
                    @if (env.is_literal) {
                      <span class="akd-badge">literal</span>
                    }
                    @if (env.is_multiline) {
                      <span class="akd-badge">multiline</span>
                    }
                    @if (env.is_locked) {
                      <span class="akd-badge">locked</span>
                    }
                    @if (!hasFlags(env)) {
                      <span class="akd-muted">—</span>
                    }
                  </td>
                  <td class="right">
                    @if (!env.is_locked) {
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="startEdit(env)"
                      >
                        Edit
                      </button>
                    }
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(env)"
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
                <div class="add-flags">
                  <label class="akd-check" title="Masked once written (INV-003)">
                    <input
                      type="checkbox"
                      name="newSecret"
                      [(ngModel)]="newSecret"
                      [disabled]="busy()"
                    />
                    secret
                  </label>
                  <label class="akd-check" title="Value used as-is, without interpolation">
                    <input
                      type="checkbox"
                      name="newLiteral"
                      [(ngModel)]="newLiteral"
                      [disabled]="busy()"
                    />
                    literal
                  </label>
                </div>
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
        These are the stack's own variables — what <code class="akd-mono">$&#123;VAR&#125;</code> in
        the compose file resolves against. Shared variables from the team, project, environment and
        server are added on top, the stack's own value winning. Applied at the next deployment.
      </p>
    }
  `,
  styles: [
    `
      .hint {
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
      .add-flags {
        display: flex;
        gap: var(--space-3);
        flex-wrap: wrap;
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
export class ServiceEnvsComponent {
  readonly serviceUuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly envs = signal<EnvVar[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected newKey = '';
  protected newValue = '';
  protected newSecret = false;
  protected newLiteral = false;
  /** UUID of the row open for editing — one at a time. */
  protected readonly editing = signal<string | null>(null);
  protected editValue = '';
  protected editLiteral = false;

  constructor() {
    effect(() => {
      const uuid = this.serviceUuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      await this.reload(uuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private async reload(uuid: string = this.serviceUuid()): Promise<void> {
    const page = await fetchAll((cursor) =>
      this.api.client().listServiceEnvs(uuid, { limit: 100, cursor }),
    );
    this.envs.set([...page].sort((a, b) => a.key.localeCompare(b.key)));
  }

  protected hasFlags(env: EnvVar): boolean {
    return Boolean(env.is_secret || env.is_literal || env.is_multiline || env.is_locked);
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.newKey.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createServiceEnv(this.serviceUuid(), {
        key: this.newKey.trim(),
        value: this.newValue,
        is_secret: this.newSecret,
        // A stack builds from its compose file, not from a build pack: there is
        // no build-time set to put a variable in, and locking is a later choice.
        is_build_time: false,
        is_literal: this.newLiteral,
        is_multiline: this.newValue.includes('\n'),
        is_locked: false,
      });
      this.newKey = '';
      this.newValue = '';
      this.newSecret = false;
      this.newLiteral = false;
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected startEdit(env: EnvVar): void {
    this.editing.set(env.uuid);
    this.editValue = resourceEnvEditValue(env);
    this.editLiteral = env.is_literal;
    this.error.set(null);
  }

  /** The value only reaches the API when it changed — a redacted variable left
   * untouched keeps the value nobody could read. */
  protected async save(env: EnvVar): Promise<void> {
    if (this.busy()) return;
    const body = resourceEnvUpdatePayload(env, {
      value: this.editValue,
      literal: this.editLiteral,
    });
    if (!body) {
      this.editing.set(null);
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateServiceEnv(this.serviceUuid(), env.uuid, body);
      this.editing.set(null);
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(env: EnvVar): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the variable',
        message: `Delete "${env.key}" from this stack? The containers pick it up at the next deployment.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteServiceEnv(this.serviceUuid(), env.uuid);
      await this.reload();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
