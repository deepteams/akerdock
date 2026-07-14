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
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type EnvVar = components['schemas']['EnvironmentVariable'];

@Component({
  selector: 'app-application-envs-tab',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <form class="akd-card create" (ngSubmit)="create()">
      <div class="akd-field">
        <label for="ev-key">Key</label>
        <input
          id="ev-key"
          name="key"
          class="akd-input akd-mono"
          required
          [(ngModel)]="key"
          [disabled]="busy()"
        />
      </div>
      <div class="akd-field">
        <label for="ev-value">Value</label>
        <textarea
          id="ev-value"
          name="value"
          class="akd-textarea"
          rows="3"
          [(ngModel)]="value"
          [disabled]="busy()"
        ></textarea>
      </div>
      <label class="check">
        <input type="checkbox" name="isSecret" [(ngModel)]="isSecret" [disabled]="busy()" />
        Secret (redacted after write; passed as a BuildKit secret, never a build arg)
      </label>
      <label class="check">
        <input type="checkbox" name="isBuildTime" [(ngModel)]="isBuildTime" [disabled]="busy()" />
        Available at build time
      </label>
      <div>
        <button class="akd-btn" type="submit" [disabled]="busy() || !key.trim()">
          Add variable
        </button>
      </div>
    </form>

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (envs().length === 0) {
      <div class="akd-empty">
        <p><strong>No environment variables.</strong></p>
      </div>
    } @else {
      <table class="akd-table">
        <caption class="sr-only">Environment variables of this application</caption>
        <thead>
          <tr>
            <th scope="col">Key</th>
            <th scope="col">Value</th>
            <th scope="col">Flags</th>
            <th scope="col"><span class="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          @for (env of envs(); track env.uuid) {
            <tr>
              <td class="akd-mono">{{ env.key }}</td>
              <td>
                @if (editing() === env.uuid) {
                  <form class="edit" (ngSubmit)="saveEdit(env)">
                    <textarea
                      class="akd-textarea"
                      name="editValue"
                      rows="2"
                      [attr.aria-label]="'New value for ' + env.key"
                      [(ngModel)]="editValue"
                      [disabled]="busy()"
                    ></textarea>
                    <div class="edit-actions">
                      <button class="akd-btn" type="submit" [disabled]="busy()">Save</button>
                      <button class="akd-btn-ghost" type="button" (click)="editing.set(null)">
                        Cancel
                      </button>
                    </div>
                  </form>
                } @else {
                  <!-- A redacted value is redacted for good: the API never returns
                       it again, so there is no "reveal" to offer here. -->
                  <span class="akd-mono">{{ env.is_redacted ? '(redacted)' : env.value }}</span>
                }
              </td>
              <td class="akd-muted">{{ flags(env) }}</td>
              <td class="right">
                @if (!env.is_locked) {
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="startEdit(env)"
                  >
                    Edit
                  </button>
                }
                <button
                  class="akd-btn-danger"
                  type="button"
                  [disabled]="busy()"
                  (click)="remove(env)"
                >
                  Delete
                </button>
              </td>
            </tr>
          }
        </tbody>
      </table>
    }
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--akd-space-5);
        max-width: 40rem;
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .edit {
        display: grid;
        gap: var(--akd-space-2);
      }
      .edit-actions {
        display: flex;
        gap: var(--akd-space-2);
      }
    `,
  ],
})
export class ApplicationEnvsTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly envs = signal<EnvVar[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly editing = signal<string | null>(null);

  protected key = '';
  protected value = '';
  protected isSecret = false;
  protected isBuildTime = false;
  protected editValue = '';

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected flags(env: EnvVar): string {
    const flags = [
      env.is_secret ? 'secret' : null,
      env.is_build_time ? 'build' : null,
      env.is_literal ? 'literal' : null,
      env.is_multiline ? 'multiline' : null,
      env.is_locked ? 'locked' : null,
    ].filter(Boolean);
    return flags.length ? flags.join(', ') : '—';
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const page = await this.api.client().listApplicationEnvs(uuid, { limit: 100 });
      this.envs.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.key.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createApplicationEnv(this.uuid(), {
        key: this.key.trim(),
        value: this.value,
        is_secret: this.isSecret,
        is_build_time: this.isBuildTime,
        is_literal: false,
        is_multiline: this.value.includes('\n'),
        is_locked: false,
      });
      this.key = '';
      this.value = '';
      this.isSecret = false;
      this.isBuildTime = false;
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected startEdit(env: EnvVar): void {
    this.editing.set(env.uuid);
    // A redacted value cannot be prefilled — the page never had it.
    this.editValue = env.is_redacted ? '' : (env.value ?? '');
  }

  protected async saveEdit(env: EnvVar): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateApplicationEnv(this.uuid(), env.uuid, {
        value: this.editValue,
      });
      this.editing.set(null);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(env: EnvVar): Promise<void> {
    if (
      !confirm(
        `Delete the variable "${env.key}"? The next deployment will run without it.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteApplicationEnv(this.uuid(), env.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
