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
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type EnvVar = components['schemas']['EnvironmentVariable'];

@Component({
  selector: 'app-application-envs-tab',
  standalone: true,
  imports: [FormsModule, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <akd-card title="Add variable" class="create">
      <form class="form" (ngSubmit)="create()">
        <div class="akd-field">
          <label class="akd-field__label" for="ev-key">Key</label>
          <input
            id="ev-key"
            name="key"
            class="akd-input akd-input--mono"
            required
            [(ngModel)]="key"
            [disabled]="busy()"
          />
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="ev-value">Value</label>
          <textarea
            id="ev-value"
            name="value"
            class="akd-textarea"
            rows="3"
            [(ngModel)]="value"
            [disabled]="busy()"
          ></textarea>
        </div>
        <label class="akd-check">
          <input type="checkbox" name="isSecret" [(ngModel)]="isSecret" [disabled]="busy()" />
          Secret (redacted after write; passed as a BuildKit secret, never a build arg)
        </label>
        <label class="akd-check">
          <input type="checkbox" name="isBuildTime" [(ngModel)]="isBuildTime" [disabled]="busy()" />
          Available at build time
        </label>
        <div>
          <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy() || !key.trim()">
            <akd-icon name="plus" [size]="15" />
            Add variable
          </button>
        </div>
      </form>
    </akd-card>

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (envs().length === 0) {
      <akd-empty-state icon="key-round" title="No environment variables" />
    } @else {
      <akd-card title="Environment variables" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Environment variables of this application
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
                        <button
                          class="akd-btn akd-btn--primary akd-btn--sm"
                          type="submit"
                          [disabled]="busy()"
                        >
                          Save
                        </button>
                        <button
                          class="akd-btn akd-btn--secondary akd-btn--sm"
                          type="button"
                          (click)="editing.set(null)"
                        >
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
                <td>
                  @if (env.is_secret) {
                    <span class="akd-badge akd-badge--accent">secret</span>
                  }
                  @if (env.is_build_time) {
                    <span class="akd-badge akd-badge--mono">build</span>
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
                  <div class="row-actions">
                    @if (!env.is_locked) {
                      <button
                        class="akd-btn akd-btn--ghost akd-btn--sm"
                        type="button"
                        [disabled]="busy()"
                        (click)="startEdit(env)"
                      >
                        <akd-icon name="pencil" [size]="13" />
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
                  </div>
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }
  `,
  styles: [
    `
      .create {
        display: block;
        margin-bottom: var(--space-5);
        max-width: 40rem;
      }
      .form {
        display: grid;
        gap: var(--space-3);
      }
      .edit {
        display: grid;
        gap: var(--space-2);
        padding: var(--space-2) 0;
      }
      .edit-actions {
        display: flex;
        gap: var(--space-2);
      }
      .row-actions {
        display: flex;
        gap: var(--space-2);
        justify-content: flex-end;
      }
      .akd-badge + .akd-badge {
        margin-left: var(--space-1);
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

  protected hasFlags(env: EnvVar): boolean {
    return !!(
      env.is_secret ||
      env.is_build_time ||
      env.is_literal ||
      env.is_multiline ||
      env.is_locked
    );
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
    if (!confirm(`Delete the variable "${env.key}"? The next deployment will run without it.`)) {
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
