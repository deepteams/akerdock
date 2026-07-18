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
          <label class="check">
            <input
              type="checkbox"
              name="autoDeploy"
              [(ngModel)]="form.autoDeploy"
              [disabled]="busy()"
            />
            Auto-deploy on push (via the webhook endpoint)
          </label>
        }

        @if (application()!.source_type === 'git') {
          <fieldset class="previews">
            <legend>PR previews</legend>
            <label class="check">
              <input
                type="checkbox"
                name="previewsEnabled"
                [(ngModel)]="form.previewsEnabled"
                [disabled]="busy()"
              />
              Deploy a protected preview for every pull request (GitHub App source)
            </label>
            @if (form.previewsEnabled) {
              <div class="akd-field">
                <label for="pv-template">URL template ({{ templateHint }})</label>
                <input
                  id="pv-template"
                  name="previewUrlTemplate"
                  class="akd-input akd-mono"
                  [(ngModel)]="form.previewUrlTemplate"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="pv-max">Max concurrent previews (empty = unlimited)</label>
                <input
                  id="pv-max"
                  name="previewMaxConcurrent"
                  class="akd-input akd-mono"
                  inputmode="numeric"
                  [(ngModel)]="form.previewMaxConcurrent"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="pv-ttl">Inactivity TTL in minutes (empty = never destroyed)</label>
                <input
                  id="pv-ttl"
                  name="previewTtlMinutes"
                  class="akd-input akd-mono"
                  inputmode="numeric"
                  [(ngModel)]="form.previewTtlMinutes"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="pv-protection">Access protection</label>
                <select
                  id="pv-protection"
                  name="previewProtection"
                  class="akd-select"
                  [(ngModel)]="form.previewProtection"
                  [disabled]="busy()"
                >
                  <option value="basic_auth">Basic auth (default)</option>
                  <option value="none">Public (explicit choice)</option>
                </select>
              </div>
              <label class="check">
                <input
                  type="checkbox"
                  name="previewForkApproval"
                  [(ngModel)]="form.previewForkApprovalEnabled"
                  [disabled]="busy()"
                />
                Allow fork PRs after maintainer approval (no secret ever injected)
              </label>
              <label class="check">
                <input
                  type="checkbox"
                  name="previewExcludeDrafts"
                  [(ngModel)]="form.previewExcludeDrafts"
                  [disabled]="busy()"
                />
                Skip draft pull requests
              </label>
              <div class="akd-field">
                <label for="pv-label">Required PR label (empty = every PR gets a preview)</label>
                <input
                  id="pv-label"
                  name="previewRequireLabel"
                  class="akd-input akd-mono"
                  [(ngModel)]="form.previewRequireLabel"
                  [disabled]="busy()"
                />
              </div>
              <label class="check">
                <input
                  type="checkbox"
                  name="previewCommentCommands"
                  [(ngModel)]="form.previewCommentCommandsEnabled"
                  [disabled]="busy()"
                />
                Enable /deploy and /destroy comment commands on pull requests
              </label>
              <label class="check">
                <input
                  type="checkbox"
                  name="previewCancelObsoleteBuilds"
                  [(ngModel)]="form.previewCancelObsoleteBuilds"
                  [disabled]="busy()"
                />
                Cancel the preview build made obsolete by a new commit
              </label>
              <div class="akd-field">
                <label for="pv-token">
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
                <label class="check">
                  <input
                    type="checkbox"
                    name="gitApiTokenClear"
                    [(ngModel)]="form.gitApiTokenClear"
                    [disabled]="busy()"
                  />
                  Remove the configured token
                </label>
              }
              <p class="akd-muted hint">
                The token is write-only: encrypted at rest, never returned by the API. Comment
                commands and GitLab/Gitea preview feedback (commit statuses, PR comment) need it
                for manual webhook sources — not needed with a GitHub App.
              </p>
            }
          </fieldset>
        }

        <div class="actions">
          <button class="akd-btn" type="submit" [disabled]="busy() || !form.name.trim()">
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
        gap: var(--akd-space-3);
      }
      .previews {
        margin: 0;
        padding: var(--akd-space-3) var(--akd-space-4) var(--akd-space-4);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        display: grid;
        gap: var(--akd-space-3);
      }
      .previews legend {
        padding: 0 var(--akd-space-2);
        font-size: var(--akd-text-xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.04em;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .actions {
        display: flex;
        align-items: center;
        gap: var(--akd-space-3);
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
