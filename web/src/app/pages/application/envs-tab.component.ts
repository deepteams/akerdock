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
import { parseDotenv, quoteEnvValue, REDACTED } from './dotenv';
import type { components } from '../../../api/schema';

type EnvVar = components['schemas']['EnvironmentVariable'];

@Component({
  selector: 'app-application-envs-tab',
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
      <akd-card
        [title]="
          previewUuid() ? 'Environment variables · this PR' : 'Environment variables · ' + set()
        "
        [padded]="false"
      >
        <div class="toolbar">
          <!-- Two DISTINCT sets (INV-010): previews never inherit production —
               this switcher is where a PR instance's keys get defined. -->
          @if (!previewUuid()) {
            <button
              type="button"
              class="akd-btn akd-btn--sm"
              [class.akd-btn--secondary]="set() === 'production'"
              [class.akd-btn--ghost]="set() !== 'production'"
              (click)="switchSet('production')"
            >
              Production
            </button>
            <button
              type="button"
              class="akd-btn akd-btn--sm"
              [class.akd-btn--secondary]="set() === 'previews'"
              [class.akd-btn--ghost]="set() !== 'previews'"
              (click)="switchSet('previews')"
            >
              Previews
            </button>
            <span class="sep"></span>
          }
          <button
            type="button"
            class="akd-btn akd-btn--sm"
            [class.akd-btn--secondary]="view() === 'table'"
            [class.akd-btn--ghost]="view() !== 'table'"
            (click)="view.set('table')"
          >
            Table
          </button>
          <button
            type="button"
            class="akd-btn akd-btn--sm"
            [class.akd-btn--secondary]="view() === 'run'"
            [class.akd-btn--ghost]="view() !== 'run'"
            (click)="openDev('run')"
          >
            Runtime · .env
          </button>
          <button
            type="button"
            class="akd-btn akd-btn--sm"
            [class.akd-btn--secondary]="view() === 'build'"
            [class.akd-btn--ghost]="view() !== 'build'"
            (click)="openDev('build')"
          >
            Build · .env
          </button>
          <span class="spacer"></span>
          <!-- Masked by DEFAULT: this page must be safe to open during a
               screen share — revealing is the explicit gesture, never the
               landing state. -->
          <button
            type="button"
            class="akd-btn akd-btn--ghost akd-btn--sm"
            [attr.aria-pressed]="revealed()"
            (click)="revealed.set(!revealed())"
          >
            <akd-icon [name]="revealed() ? 'eye-off' : 'eye'" [size]="13" />
            {{ revealed() ? 'Hide values' : 'Show values' }}
          </button>
        </div>
        @if (view() !== 'table') {
          <div class="dev">
            <p class="akd-muted">
              One KEY=value per line — paste a whole .env at once. Keeping a "(redacted)" line
              leaves that secret untouched; removing a line deletes the variable. New variables are
              created
              {{ view() === 'build' ? 'as build-time' : 'as runtime' }} and non-secret.
            </p>
            <textarea
              class="akd-textarea akd-mono"
              name="devText"
              rows="14"
              [attr.aria-label]="(view() === 'build' ? 'Build' : 'Runtime') + ' variables as .env'"
              [(ngModel)]="devText"
              [disabled]="busy()"
            ></textarea>
            <div class="edit-actions">
              <button
                class="akd-btn akd-btn--primary akd-btn--sm"
                type="button"
                [disabled]="busy()"
                (click)="applyDev()"
              >
                Apply changes
              </button>
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                [disabled]="busy()"
                (click)="view.set('table')"
              >
                Cancel
              </button>
            </div>
          </div>
        } @else {
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
                         it again, so there is no "reveal" to offer here. Everything
                         else is masked until the operator asks to see it. -->
                      <span class="akd-mono">{{
                        env.is_redacted ? '(redacted)' : revealed() ? env.value : '••••••••'
                      }}</span>
                    }
                  </td>
                  <td>
                    @if (env.is_preview_override) {
                      <span class="akd-badge akd-badge--accent">PR override</span>
                    }
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
                      @if (!previewUuid() || env.is_preview_override) {
                        <button
                          class="akd-btn akd-btn--danger akd-btn--sm"
                          type="button"
                          [disabled]="busy()"
                          (click)="remove(env)"
                        >
                          {{
                            previewUuid() && env.is_preview_override ? 'Remove override' : 'Delete'
                          }}
                        </button>
                      } @else {
                        <span class="akd-muted">shared set</span>
                      }
                    </div>
                  </td>
                </tr>
              }
              <!-- The last row IS the creator: add a variable in place, or paste a
                 whole .env from the Runtime/Build views above. -->
              <tr class="add-row">
                <td>
                  <input
                    class="akd-input akd-input--mono"
                    name="newKey"
                    placeholder="NEW_KEY"
                    aria-label="New variable key"
                    [(ngModel)]="key"
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
                    [(ngModel)]="value"
                    [disabled]="busy()"
                    (keydown.enter)="create()"
                  />
                </td>
                <td>
                  <div class="add-flags">
                    <label
                      class="akd-check"
                      title="Redacted after write; passed as a BuildKit secret, never a build arg"
                    >
                      <input
                        type="checkbox"
                        name="newSecret"
                        [(ngModel)]="isSecret"
                        [disabled]="busy()"
                      />
                      secret
                    </label>
                    <label class="akd-check" title="Available at build time">
                      <input
                        type="checkbox"
                        name="newBuild"
                        [(ngModel)]="isBuildTime"
                        [disabled]="busy()"
                      />
                      build
                    </label>
                  </div>
                </td>
                <td class="right">
                  <button
                    class="akd-btn akd-btn--primary akd-btn--sm"
                    type="button"
                    [disabled]="busy() || !key.trim()"
                    (click)="create()"
                  >
                    <akd-icon name="plus" [size]="13" />
                    Add
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        }
      </akd-card>
    }
  `,
  styles: [
    `
      .add-row td {
        vertical-align: middle;
      }
      .add-row .akd-input {
        width: 100%;
      }
      .add-flags {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-3);
      }
      .add-flags .akd-check {
        font-size: var(--text-xs);
        white-space: nowrap;
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
      .toolbar {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        padding: var(--space-3);
        border-bottom: 1px solid var(--border);
      }
      .toolbar .spacer {
        flex: 1;
      }
      .toolbar .sep {
        width: 1px;
        align-self: stretch;
        background: var(--border);
      }
      .dev {
        display: grid;
        gap: var(--space-3);
        padding: var(--space-3);
      }
      .akd-badge + .akd-badge {
        margin-left: var(--space-1);
      }
    `,
  ],
})
export class ApplicationEnvsTabComponent {
  readonly uuid = input.required<string>();
  /** When set, the tab shows the EFFECTIVE variables of that PR instance
   * (shared preview set + this PR's overrides) and every mutation targets
   * the PR: creates become overrides, editing a shared row forks it into an
   * override, and shared rows cannot be deleted from here. */
  readonly previewUuid = input<string | undefined>(undefined);

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly envs = signal<EnvVar[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly editing = signal<string | null>(null);
  protected readonly view = signal<'table' | 'run' | 'build'>('table');
  /** Which variable set is on screen: production, or the previews' own
   * (INV-010 — a PR instance never inherits production values). */
  protected readonly set = signal<'production' | 'previews'>('production');
  /** Values are masked on landing — safe to open while screen sharing. */
  protected readonly revealed = signal(false);

  protected key = '';
  protected value = '';
  protected isSecret = false;
  protected isBuildTime = false;
  protected editValue = '';
  protected devText = '';

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

  protected switchSet(target: 'production' | 'previews'): void {
    if (this.set() === target) return;
    this.set.set(target);
    this.view.set('table');
    void this.load(this.uuid());
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const previewUuid = this.previewUuid();
      const envs = previewUuid
        ? (await this.api.client().listPreviewEnvs(uuid, previewUuid)).data
        : await fetchAll((cursor) =>
            this.api.client().listApplicationEnvs(uuid, {
              limit: 100,
              cursor,
              preview: this.set() === 'previews',
            }),
          );
      this.envs.set(envs);
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
      const create = {
        key: this.key.trim(),
        value: this.value,
        is_secret: this.isSecret,
        is_build_time: this.isBuildTime,
        is_literal: false,
        is_multiline: this.value.includes('\n'),
        is_locked: false,
      };
      const previewUuid = this.previewUuid();
      if (previewUuid) {
        await this.api.client().createPreviewEnv(this.uuid(), previewUuid, create);
      } else {
        await this.api
          .client()
          .createApplicationEnv(this.uuid(), create, { preview: this.set() === 'previews' });
      }
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
      const previewUuid = this.previewUuid();
      if (previewUuid && !env.is_preview_override) {
        // Forking, not editing: the shared row serves every preview — this
        // value must live and die with THIS PR only.
        await this.api.client().createPreviewEnv(this.uuid(), previewUuid, {
          key: env.key,
          value: this.editValue,
          is_secret: env.is_secret ?? false,
          is_build_time: env.is_build_time,
          is_literal: env.is_literal,
          is_multiline: this.editValue.includes('\n'),
          is_locked: false,
        });
      } else {
        await this.api.client().updateApplicationEnv(this.uuid(), env.uuid, {
          value: this.editValue,
        });
      }
      this.editing.set(null);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /** Variables of one dev view: locked ones stay out — they are not editable
   * anywhere in this tab, so the raw text must not offer to delete them. */
  private devGroup(mode: 'run' | 'build'): EnvVar[] {
    return this.envs().filter(
      (env) => !!env.is_build_time === (mode === 'build') && !env.is_locked,
    );
  }

  protected openDev(mode: 'run' | 'build'): void {
    this.devText = this.devGroup(mode)
      .map((env) => `${env.key}=${quoteEnvValue(env.is_redacted ? REDACTED : (env.value ?? ''))}`)
      .join('\n');
    this.view.set(mode);
  }

  protected async applyDev(): Promise<void> {
    const mode = this.view();
    if (mode === 'table' || this.busy()) return;
    const parsed = parseDotenv(this.devText);
    if (parsed.errors.length > 0) {
      this.error.set(`Not a valid .env: ${parsed.errors.join(' — ')}`);
      return;
    }

    const current = this.devGroup(mode);
    const byKey = new Map(current.map((env) => [env.key, env]));
    const creates: { key: string; value: string }[] = [];
    const updates: { env: EnvVar; value: string }[] = [];
    for (const [key, value] of parsed.entries) {
      const existing = byKey.get(key);
      if (!existing) {
        creates.push({ key, value });
      } else if (existing.is_redacted ? value !== REDACTED : value !== (existing.value ?? '')) {
        updates.push({ env: existing, value });
      }
    }
    let deletes = current.filter((env) => !parsed.entries.has(env.key));
    if (this.previewUuid()) {
      // From the PR page only its own overrides can go — a shared key
      // missing from the pasted text is not this page's to delete.
      deletes = deletes.filter((env) => env.is_preview_override);
    }
    if (creates.length === 0 && updates.length === 0 && deletes.length === 0) {
      this.view.set('table');
      return;
    }
    if (
      deletes.length > 0 &&
      !(await this.confirm.ask({
        title: 'Delete the removed variables',
        message:
          `${deletes.length} variable(s) removed from the text will be deleted: ` +
          `${deletes.map((env) => env.key).join(', ')}. Continue?`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }

    this.busy.set(true);
    this.error.set(null);
    try {
      const previewUuid = this.previewUuid();
      for (const c of creates) {
        const create = {
          key: c.key,
          value: c.value,
          is_secret: false,
          is_build_time: mode === 'build',
          is_literal: false,
          is_multiline: c.value.includes('\n'),
          is_locked: false,
        };
        if (previewUuid) {
          await this.api.client().createPreviewEnv(this.uuid(), previewUuid, create);
        } else {
          await this.api
            .client()
            .createApplicationEnv(this.uuid(), create, { preview: this.set() === 'previews' });
        }
      }
      for (const u of updates) {
        if (previewUuid && !u.env.is_preview_override) {
          await this.api.client().createPreviewEnv(this.uuid(), previewUuid, {
            key: u.env.key,
            value: u.value,
            is_secret: u.env.is_secret ?? false,
            is_build_time: u.env.is_build_time,
            is_literal: u.env.is_literal,
            is_multiline: u.value.includes('\n'),
            is_locked: false,
          });
        } else {
          await this.api.client().updateApplicationEnv(this.uuid(), u.env.uuid, { value: u.value });
        }
      }
      for (const d of deletes) {
        await this.api.client().deleteApplicationEnv(this.uuid(), d.uuid);
      }
      await this.load(this.uuid());
      this.view.set('table');
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
        message: `Delete the variable "${env.key}"? The next deployment will run without it.`,
        confirmLabel: 'Delete',
      }))
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
