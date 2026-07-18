import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  output,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../../core/api.service';
import { ApiError } from '../../../api/client';
import type { components } from '../../../api/schema';
import { ApplicationConfigFieldsComponent } from './config-fields.component';
import { emptyConfigForm, settingsFromApplication, settingsToUpdate } from './application-form';
import type { SettingsForm } from './application-form';

type Application = components['schemas']['Application'];

/** Placeholders of preview_url_template — a literal {{ cannot be written in
 *  an Angular template, so the hint is a plain string. */
const PREVIEW_TEMPLATE_HINT = '{{pr_id}}, {{domain}}, {{random}}';
type RegistryCredential = components['schemas']['RegistryCredential'];
type PrivateKey = components['schemas']['PrivateKey'];

/**
 * The FULL configuration of an application, editable in one form: every field
 * of the PATCH contract is here, so nothing requires falling back to curl.
 */
@Component({
  selector: 'app-application-settings-tab',
  standalone: true,
  imports: [FormsModule, ApplicationConfigFieldsComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }
    @if (notice(); as message) {
      <p class="akd-muted" role="status">{{ message }}</p>
    }

    @if (!application()) {
      <p class="akd-muted">Loading…</p>
    } @else {
      <form class="form" (ngSubmit)="save()">
        <app-application-config-fields
          [form]="form"
          [sourceType]="application()!.source_type"
          [registries]="registries()"
          [privateKeys]="privateKeys()"
          [busy]="busy()"
        />

        @if (application()!.source_type === 'git') {
          <section class="akd-card group">
            <div class="akd-card__header">
              <h2 class="akd-card__title">Deploys</h2>
            </div>
            <div class="akd-card__body body">
              <label class="switch">
                <input
                  type="checkbox"
                  class="akd-switch"
                  name="autoDeploy"
                  [(ngModel)]="form.autoDeploy"
                  [disabled]="busy()"
                />
                <span>
                  <span class="switch__label">Auto-deploy on push</span>
                  <span class="switch__desc">Via the webhook endpoint.</span>
                </span>
              </label>
            </div>
          </section>

          <section class="akd-card group">
            <div class="akd-card__header">
              <h2 class="akd-card__title">PR previews</h2>
            </div>
            <div class="akd-card__body body">
              <label class="switch">
                <input
                  type="checkbox"
                  class="akd-switch"
                  name="previewsEnabled"
                  [(ngModel)]="form.previewsEnabled"
                  [disabled]="busy()"
                />
                <span>
                  <span class="switch__label">PR previews</span>
                  <span class="switch__desc">
                    Deploy a protected preview for every pull request (GitHub App source).
                  </span>
                </span>
              </label>
              @if (form.previewsEnabled) {
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-template">
                    URL template ({{ templateHint }})
                  </label>
                  <input
                    id="pv-template"
                    name="previewUrlTemplate"
                    class="akd-input akd-input--mono"
                    [(ngModel)]="form.previewUrlTemplate"
                    [disabled]="busy()"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-max">
                    Max concurrent previews (empty = unlimited)
                  </label>
                  <input
                    id="pv-max"
                    name="previewMaxConcurrent"
                    class="akd-input akd-input--mono"
                    inputmode="numeric"
                    [(ngModel)]="form.previewMaxConcurrent"
                    [disabled]="busy()"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-ttl">
                    Inactivity TTL in minutes (empty = never destroyed)
                  </label>
                  <input
                    id="pv-ttl"
                    name="previewTtlMinutes"
                    class="akd-input akd-input--mono"
                    inputmode="numeric"
                    [(ngModel)]="form.previewTtlMinutes"
                    [disabled]="busy()"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-protection">Access protection</label>
                  <div class="akd-select">
                    <select
                      id="pv-protection"
                      name="previewProtection"
                      class="akd-input"
                      [(ngModel)]="form.previewProtection"
                      [disabled]="busy()"
                    >
                      <option value="basic_auth">Basic auth (default)</option>
                      <option value="none">Public (explicit choice)</option>
                    </select>
                  </div>
                </div>
                <label class="switch">
                  <input
                    type="checkbox"
                    class="akd-switch"
                    name="previewForkApproval"
                    [(ngModel)]="form.previewForkApprovalEnabled"
                    [disabled]="busy()"
                  />
                  <span>
                    <span class="switch__label">Fork PRs after approval</span>
                    <span class="switch__desc">
                      Allow fork PRs after maintainer approval (no secret ever injected).
                    </span>
                  </span>
                </label>
                <label class="switch">
                  <input
                    type="checkbox"
                    class="akd-switch"
                    name="previewExcludeDrafts"
                    [(ngModel)]="form.previewExcludeDrafts"
                    [disabled]="busy()"
                  />
                  <span>
                    <span class="switch__label">Skip draft pull requests</span>
                  </span>
                </label>
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-label">
                    Required PR label (empty = every PR gets a preview)
                  </label>
                  <input
                    id="pv-label"
                    name="previewRequireLabel"
                    class="akd-input akd-input--mono"
                    [(ngModel)]="form.previewRequireLabel"
                    [disabled]="busy()"
                  />
                </div>
                <label class="switch">
                  <input
                    type="checkbox"
                    class="akd-switch"
                    name="previewCommentCommands"
                    [(ngModel)]="form.previewCommentCommandsEnabled"
                    [disabled]="busy()"
                  />
                  <span>
                    <span class="switch__label">Comment commands</span>
                    <span class="switch__desc">
                      Enable /deploy and /destroy comment commands on pull requests.
                    </span>
                  </span>
                </label>
                <label class="switch">
                  <input
                    type="checkbox"
                    class="akd-switch"
                    name="previewCancelObsoleteBuilds"
                    [(ngModel)]="form.previewCancelObsoleteBuilds"
                    [disabled]="busy()"
                  />
                  <span>
                    <span class="switch__label">Cancel obsolete builds</span>
                    <span class="switch__desc">
                      Cancel the preview build made obsolete by a new commit.
                    </span>
                  </span>
                </label>
                <div class="akd-field">
                  <label class="akd-field__label" for="pv-token">
                    Provider API token (GitLab / Gitea PAT)
                    @if (application()!.git_api_token_set) {
                      <span class="akd-muted">— a token is configured</span>
                    }
                  </label>
                  <input
                    id="pv-token"
                    name="gitApiToken"
                    type="password"
                    class="akd-input"
                    autocomplete="new-password"
                    [placeholder]="application()!.git_api_token_set ? 'leave blank to keep' : ''"
                    [(ngModel)]="form.gitApiToken"
                    [disabled]="busy() || form.gitApiTokenClear"
                  />
                </div>
                @if (application()!.git_api_token_set) {
                  <label class="akd-check">
                    <input
                      type="checkbox"
                      name="gitApiTokenClear"
                      [(ngModel)]="form.gitApiTokenClear"
                      [disabled]="busy()"
                    />
                    Remove the configured token
                  </label>
                }
                <p class="akd-field__hint hint">
                  The token is write-only: encrypted at rest, never returned by the API. Comment
                  commands and GitLab/Gitea preview feedback (commit statuses, PR comment) need it
                  for manual webhook sources — not needed with a GitHub App.
                </p>
              }
            </div>
          </section>
        }

        <div class="actions">
          <button
            class="akd-btn akd-btn--primary"
            type="submit"
            [disabled]="busy() || !form.name.trim()"
          >
            {{ busy() ? 'Saving…' : 'Save settings' }}
          </button>
          <span class="akd-muted">
            Changes apply at the next deployment — domains immediately.
          </span>
        </div>
      </form>
    }
  `,
  styles: [
    `
      .form {
        max-width: 44rem;
        display: grid;
        gap: var(--space-4);
      }
      .body {
        display: grid;
        gap: var(--space-4);
      }
      .switch {
        display: flex;
        align-items: flex-start;
        gap: var(--space-3);
        cursor: pointer;
      }
      .switch > .akd-switch {
        margin-top: var(--space-1);
      }
      .switch__label {
        display: block;
        font-weight: var(--weight-medium);
        color: var(--text-1);
      }
      .switch__desc {
        display: block;
        font-size: var(--text-sm);
        color: var(--text-3);
      }
      .hint {
        margin: 0;
      }
      .actions {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
    `,
  ],
})
export class ApplicationSettingsTabComponent {
  readonly uuid = input.required<string>();
  /** Emits the updated application so the parent header stays truthful. */
  readonly saved = output<Application>();

  private readonly api = inject(ApiService);

  protected readonly templateHint = PREVIEW_TEMPLATE_HINT;
  protected readonly application = signal<Application | null>(null);
  protected readonly registries = signal<RegistryCredential[]>([]);
  protected readonly privateKeys = signal<PrivateKey[]>([]);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected form: SettingsForm = {
    ...emptyConfigForm(),
    autoDeploy: false,
    previewsEnabled: false,
    previewUrlTemplate: '{{pr_id}}.{{domain}}',
    previewMaxConcurrent: '',
    previewTtlMinutes: '',
    previewProtection: 'basic_auth',
    previewForkApprovalEnabled: false,
    previewExcludeDrafts: false,
    previewRequireLabel: '',
    previewCommentCommandsEnabled: false,
    previewCancelObsoleteBuilds: false,
    gitApiToken: '',
    gitApiTokenClear: false,
  };

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    const client = this.api.client();
    try {
      const [app, registries, keys] = await Promise.all([
        client.getApplication(uuid),
        client.listRegistryCredentials({ limit: 100 }),
        client.listPrivateKeys({ limit: 100 }),
      ]);
      this.setApplication(app);
      this.registries.set(registries.data);
      this.privateKeys.set(keys.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  private setApplication(app: Application): void {
    this.application.set(app);
    this.form = settingsFromApplication(app);
  }

  protected async save(): Promise<void> {
    const app = this.application();
    if (!app || this.busy() || !this.form.name.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api
        .client()
        .updateApplication(this.uuid(), app.version, settingsToUpdate(this.form, app.source_type));
      this.setApplication(updated);
      this.saved.emit(updated);
      this.notice.set('Settings saved — applied at the next deployment (domains immediately).');
    } catch (err) {
      // A 409 version conflict means someone else changed the configuration
      // while this form was open. Reload their version rather than silently
      // clobbering it (§24.1) — the operator re-applies on top of the truth.
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.load(this.uuid());
        this.error.set(
          'Your edit raced a concurrent change: the latest configuration was reloaded. Re-apply your edit on top of it.',
        );
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }
}
