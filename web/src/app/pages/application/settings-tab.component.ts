import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  output,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import { ApiError } from '../../../api/client';
import type { components } from '../../../api/schema';
import { ApplicationConfigFieldsComponent } from './config-fields.component';
import type { ConfigSection } from './config-fields.component';
import { emptyConfigForm, settingsFromApplication, settingsToUpdate } from './application-form';
import type { SettingsForm } from './application-form';

type Application = components['schemas']['Application'];

/** Placeholders of preview_url_template — a literal {{ cannot be written in
 *  an Angular template, so the hint is a plain string. */
const PREVIEW_TEMPLATE_HINT = '{{pr_id}}, {{domain}}, {{random}}';
type RegistryCredential = components['schemas']['RegistryCredential'];
type PrivateKey = components['schemas']['PrivateKey'];

/** A navigable settings section: the config-fields ones plus the git-only ones. */
type SettingsSection = ConfigSection | 'deploys' | 'previews';

/**
 * The FULL configuration of an application, editable in one form: every field
 * of the PATCH contract is here, so nothing requires falling back to curl. A
 * left menu navigates the sections; the form (and its single Save) spans them
 * all — moving between sections never loses an unsaved edit.
 */
@Component({
  selector: 'app-application-settings-tab',
  standalone: true,
  imports: [FormsModule, IconComponent, ApplicationConfigFieldsComponent],
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
      <div class="settings">
        <nav class="settings__nav" aria-label="Settings sections">
          @for (s of sections(); track s.id) {
            <button
              type="button"
              class="settings__link"
              [class.settings__link--active]="active() === s.id"
              [attr.aria-current]="active() === s.id ? 'true' : null"
              (click)="active.set(s.id)"
            >
              {{ s.label }}
            </button>
          }
        </nav>

        <div class="settings__main">
          <form class="form" (ngSubmit)="save()">
            @if (configSection(); as sec) {
              <app-application-config-fields
                [form]="form"
                [section]="sec"
                [sourceType]="application()!.source_type"
                [githubApp]="!!application()!.github_app_uuid"
                [registries]="registries()"
                [privateKeys]="privateKeys()"
                [busy]="busy()"
              />
            }

            @if (active() === 'resources') {
              <section class="akd-card group">
                <div class="akd-card__header">
                  <h2 class="akd-card__title">Scale to zero</h2>
                </div>
                <div class="akd-card__body body">
                  <label class="switch">
                    <input
                      type="checkbox"
                      class="akd-switch"
                      name="appScaleToZero"
                      [(ngModel)]="form.scaleToZero"
                      [disabled]="busy()"
                    />
                    <span>
                      <span class="switch__label">Sleep this application when idle</span>
                      <span class="switch__desc">
                        Stops the app after inactivity and wakes it on the first request
                        via the agent. Best for request-driven, low-traffic apps: the
                        first visitor after a sleep waits for the cold start (up to 60s),
                        and background workers/crons are stopped too.
                      </span>
                    </span>
                  </label>
                  @if (form.scaleToZero) {
                    <div class="akd-field">
                      <label class="akd-field__label" for="app-stz-min">
                        Sleep after (minutes of inactivity)
                      </label>
                      <input
                        id="app-stz-min"
                        class="akd-input"
                        type="number"
                        min="1"
                        name="appScaleToZeroAfterMinutes"
                        [(ngModel)]="form.scaleToZeroAfterMinutes"
                        [disabled]="busy()"
                      />
                    </div>
                  }
                </div>
              </section>
            }

            @if (active() === 'deploys') {
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
            }

            @if (active() === 'general') {
              <!-- The application's own access wall (ADR-042) — independent
                   of the PR preview protection. -->
              <section class="akd-card group">
                <div class="akd-card__header">
                  <h2 class="akd-card__title">Access protection</h2>
                </div>
                <div class="akd-card__body body">
                  <div class="akd-field">
                    <label class="akd-field__label" for="app-access">Who can reach this application</label>
                    <div class="akd-select">
                      <select
                        id="app-access"
                        name="accessProtection"
                        class="akd-input"
                        [(ngModel)]="form.accessProtection"
                        [disabled]="busy()"
                      >
                        <option value="none">Public (default)</option>
                        <option value="sso">AkerDock login (SSO — team members only)</option>
                        <option value="basic_auth">Basic auth (shared credentials)</option>
                      </select>
                    </div>
                    <p class="akd-field__hint">
                      Applies to every domain of the application, and to every routed service of a
                      compose stack. Add narrowly scoped public routes below for webhooks and HTTP
                      callbacks that cannot authenticate.
                    </p>
                  </div>
                  @if (form.accessProtection === 'basic_auth') {
                    <div class="akd-field">
                      <label class="akd-field__label" for="app-access-credentials">
                        Shared credentials, user:password (empty = keep / generate)
                      </label>
                      <input
                        id="app-access-credentials"
                        name="accessBasicAuth"
                        class="akd-input akd-input--mono"
                        autocomplete="off"
                        [(ngModel)]="form.accessBasicAuth"
                        [disabled]="busy()"
                      />
                    </div>
                  }
                  @if (form.buildPack === 'compose') {
                    <p class="akd-field__hint">
                      Public routes are owned by each Compose service. Declare
                      <code>x-akerdock.access_public_routes</code> in that service.
                    </p>
                  } @else {
                    <div class="akd-field">
                      <span class="akd-field__label">Public routes through the wall</span>
                      <table class="akd-table routes">
                        <thead>
                          <tr>
                            <th scope="col">Path</th>
                            <th scope="col">Match</th>
                            <th scope="col">Methods</th>
                            <th scope="col">Parameter allow-lists (JSON)</th>
                            <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                          </tr>
                        </thead>
                        <tbody>
                          @for (route of form.accessPublicRoutes; track $index) {
                            <tr>
                              <td>
                                <input
                                  class="akd-input akd-input--mono"
                                  [name]="'access-public-path-' + $index"
                                  [(ngModel)]="route.path"
                                  [disabled]="busy()"
                                  placeholder="/webhook/:provider/handler"
                                  aria-label="Public route path"
                                />
                              </td>
                              <td>
                                <select
                                  class="akd-input"
                                  [name]="'access-public-match-' + $index"
                                  [(ngModel)]="route.match"
                                  [disabled]="busy()"
                                  aria-label="Public route match"
                                >
                                  <option value="exact">Exact</option>
                                  <option value="template">Template (:name)</option>
                                  <option value="prefix">Prefix</option>
                                </select>
                              </td>
                              <td>
                                <input
                                  class="akd-input akd-input--mono"
                                  [name]="'access-public-methods-' + $index"
                                  [(ngModel)]="route.methods"
                                  [disabled]="busy()"
                                  placeholder="POST"
                                  aria-label="Public route methods"
                                />
                              </td>
                              <td>
                                <input
                                  class="akd-input akd-input--mono"
                                  [name]="'access-public-params-' + $index"
                                  [(ngModel)]="route.parameters"
                                  [disabled]="busy() || route.match !== 'template'"
                                  placeholder='{"provider":["stripe","github"]}'
                                  aria-label="Public route parameter allow-lists"
                                />
                              </td>
                              <td class="right">
                                <button
                                  class="akd-iconbtn"
                                  type="button"
                                  [disabled]="busy()"
                                  (click)="removeAccessPublicRoute($index)"
                                  aria-label="Remove public route"
                                >
                                  <akd-icon name="trash-2" [size]="15" />
                                </button>
                              </td>
                            </tr>
                          }
                        </tbody>
                      </table>
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm add-route"
                        type="button"
                        [disabled]="busy()"
                        (click)="addAccessPublicRoute()"
                      >
                        <akd-icon name="plus" [size]="13" />
                        Add public route
                      </button>
                      <span class="akd-field__hint">
                        Active when the wall is enabled. Methods are mandatory. Template parameters
                        match one path segment only; optional JSON allow-lists restrict their
                        values. Prefix matching is segment-bound.
                      </span>
                    </div>
                  }
                </div>
              </section>
            }

            @if (active() === 'previews') {
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
                      <span class="akd-field__label">Preview URLs</span>
                      <table class="akd-table routes">
                        <thead>
                          <tr>
                            <th scope="col">Host template</th>
                            <th scope="col" class="port-col">Port</th>
                            <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                          </tr>
                        </thead>
                        <tbody>
                          @for (row of form.previewUrlTemplates; track $index) {
                            <tr>
                              <td>
                                <input
                                  class="akd-input akd-input--mono"
                                  [name]="'pv-host-' + $index"
                                  [(ngModel)]="row.host"
                                  [disabled]="busy()"
                                  placeholder="varuna-pr{{ '{{' }}pr_id{{ '}}' }}.ad.kedric.fr"
                                  aria-label="Preview host template"
                                />
                              </td>
                              <td class="port-col">
                                <input
                                  class="akd-input akd-input--mono"
                                  inputmode="numeric"
                                  [name]="'pv-port-' + $index"
                                  [(ngModel)]="row.port"
                                  [disabled]="busy()"
                                  placeholder="default"
                                  aria-label="Preview target port"
                                />
                              </td>
                              <td class="right">
                                <button
                                  class="akd-iconbtn"
                                  type="button"
                                  [disabled]="busy()"
                                  (click)="removePreviewRoute($index)"
                                  aria-label="Remove preview route"
                                >
                                  <akd-icon name="trash-2" [size]="15" />
                                </button>
                              </td>
                            </tr>
                          }
                        </tbody>
                      </table>
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm add-route"
                        type="button"
                        [disabled]="busy()"
                        (click)="addPreviewRoute()"
                      >
                        <akd-icon name="plus" [size]="13" />
                        Add preview URL
                      </button>
                      <span class="akd-field__hint">
                        One row per preview route. Placeholders: {{ templateHint }},
                        {{ '{{' }}service{{
                          '
                        }}' }}. A row with {{ '{{' }}service{{ '}}' }} applies to every served
                        service; empty port = the default / service port. Empty table = the legacy
                        single template.
                      </span>
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
                          <option value="sso">AkerDock login (SSO — team members only)</option>
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
                    <label class="switch">
                      <input
                        type="checkbox"
                        class="akd-switch"
                        name="previewDeployOnOpen"
                        [(ngModel)]="form.previewDeployOnOpen"
                        [disabled]="busy()"
                      />
                      <span>
                        <span class="switch__label">Auto-deploy when a PR opens</span>
                        <span class="switch__desc">
                          On by default. Turn off to only reserve the preview (URL, access
                          credential) when a PR opens — the first deployment is then manual (the
                          Previews tab, or a /deploy comment), and pushes keep updating it
                          afterwards.
                        </span>
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
                    <label class="switch">
                      <input
                        type="checkbox"
                        class="akd-switch"
                        name="previewScaleToZero"
                        [(ngModel)]="form.previewScaleToZero"
                        [disabled]="busy()"
                      />
                      <span>
                        <span class="switch__label">Scale to zero (previews)</span>
                        <span class="switch__desc">
                          Sleep an idle preview (docker stop) and wake it on the first
                          request via the agent helper.
                        </span>
                      </span>
                    </label>
                    @if (form.previewScaleToZero) {
                      <div class="akd-field">
                        <label class="akd-field__label" for="pv-stz-min">
                          Sleep after (minutes of inactivity)
                        </label>
                        <input
                          id="pv-stz-min"
                          class="akd-input"
                          type="number"
                          min="1"
                          name="previewScaleToZeroAfterMinutes"
                          [(ngModel)]="form.previewScaleToZeroAfterMinutes"
                          [disabled]="busy()"
                        />
                      </div>
                    }
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
                        [placeholder]="
                          application()!.git_api_token_set ? 'leave blank to keep' : ''
                        "
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
                      commands and GitLab/Gitea preview feedback (commit statuses, PR comment) need
                      it for manual webhook sources — not needed with a GitHub App.
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
        </div>
      </div>
    }
  `,
  styles: [
    `
      /* Left menu + section pane; stacks the menu on top on narrow screens. */
      .settings {
        display: grid;
        grid-template-columns: minmax(11rem, 14rem) 1fr;
        gap: var(--space-5);
        align-items: start;
      }
      .settings__nav {
        display: flex;
        flex-direction: column;
        gap: var(--space-1);
        position: sticky;
        top: var(--space-4);
      }
      .settings__link {
        text-align: left;
        padding: var(--space-2) var(--space-3);
        border: 0;
        border-radius: var(--radius-2);
        background: transparent;
        color: var(--text-2);
        font: inherit;
        cursor: pointer;
      }
      .settings__link:hover {
        background: var(--bg-2);
        color: var(--text-1);
      }
      .settings__link:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .settings__link--active {
        background: var(--bg-2);
        color: var(--text-1);
        font-weight: var(--weight-medium);
      }
      .settings__main {
        min-width: 0;
      }
      .form {
        max-width: 44rem;
        display: grid;
        gap: var(--space-4);
      }
      .routes {
        margin-bottom: var(--space-2);
      }
      .routes .port-col {
        width: 8rem;
      }
      .routes .right {
        width: 3rem;
        text-align: right;
      }
      .routes td {
        vertical-align: middle;
      }
      .add-route {
        justify-self: start;
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
      @media (max-width: 48rem) {
        .settings {
          grid-template-columns: 1fr;
        }
        .settings__nav {
          position: static;
          flex-direction: row;
          flex-wrap: wrap;
        }
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

  protected addPreviewRoute(): void {
    this.form.previewUrlTemplates.push({ host: '', port: '' });
  }

  protected removePreviewRoute(index: number): void {
    this.form.previewUrlTemplates.splice(index, 1);
  }

  protected addAccessPublicRoute(): void {
    this.form.accessPublicRoutes.push({
      path: '',
      match: 'exact',
      methods: 'POST',
      parameters: '',
    });
  }

  protected removeAccessPublicRoute(index: number): void {
    this.form.accessPublicRoutes.splice(index, 1);
  }

  /** The open section (left menu); defaults to the always-present General. */
  protected readonly active = signal<SettingsSection>('general');

  /** The menu, built from what actually applies to this source (a Docker image
   * has no Build; a GitHub App source hides the manual Source; only git gets
   * Deploys/PR previews). */
  protected readonly sections = computed<{ id: SettingsSection; label: string }[]>(() => {
    const app = this.application();
    if (!app) return [];
    const list: { id: SettingsSection; label: string }[] = [{ id: 'general', label: 'General' }];
    if (app.source_type !== 'git' || !app.github_app_uuid) {
      list.push({ id: 'source', label: 'Source' });
    }
    if (app.source_type !== 'docker_image') list.push({ id: 'build', label: 'Build' });
    list.push(
      { id: 'routing', label: 'Routing' },
      { id: 'hooks', label: 'Deployment hooks' },
      { id: 'health', label: 'Health check' },
      { id: 'resources', label: 'Resource limits' },
    );
    if (app.source_type === 'git') {
      list.push({ id: 'deploys', label: 'Deploys' }, { id: 'previews', label: 'PR previews' });
    }
    return list;
  });

  /** The active section as a config-fields section, or null for the git-only
   * ones this tab renders itself. */
  protected readonly configSection = computed<ConfigSection | null>(() => {
    const id = this.active();
    return id === 'deploys' || id === 'previews' ? null : id;
  });

  protected form: SettingsForm = {
    ...emptyConfigForm(),
    autoDeploy: false,
    previewsEnabled: false,
    previewUrlTemplate: '{{pr_id}}.{{domain}}',
    previewUrlTemplates: [],
    previewMaxConcurrent: '',
    previewTtlMinutes: '',
    previewProtection: 'basic_auth',
    accessProtection: 'none',
    accessBasicAuth: '',
    accessPublicRoutes: [],
    previewForkApprovalEnabled: false,
    previewExcludeDrafts: false,
    previewDeployOnOpen: true,
    previewRequireLabel: '',
    previewCommentCommandsEnabled: false,
    previewCancelObsoleteBuilds: false,
    previewScaleToZero: false,
    previewScaleToZeroAfterMinutes: 30,
    scaleToZero: false,
    scaleToZeroAfterMinutes: 30,
    gitApiToken: '',
    gitApiTokenClear: false,
  };

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
    // If the open section stops applying after a reload (e.g. source changed),
    // fall back to the first available one rather than a blank pane.
    effect(() => {
      const list = this.sections();
      if (list.length > 0 && !list.some((s) => s.id === untracked(() => this.active()))) {
        this.active.set(list[0].id);
      }
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
