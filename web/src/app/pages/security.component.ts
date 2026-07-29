import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import {
  ApiService,
  LinkedIdentity,
  MfaStatus,
  OauthProviderButton,
  Passkey,
  TotpSetup,
} from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ModalComponent } from '../../ui/modal/modal.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import type { components } from '../../api/schema';

type ApiToken = components['schemas']['ApiToken'];
type ApiTokenPermission = components['schemas']['ApiTokenPermission'];

const PERMISSIONS: ApiTokenPermission[] = ['read', 'read:sensitive', 'write', 'deploy', 'root'];

/**
 * Personal settings: passkey enrolment and revocation, linked identities and
 * TOTP two-factor authentication.
 *
 * A passkey is phishing-resistant where a password is not — the signature
 * binds the origin. This page exists so an operator can get to the point of
 * never typing the password again. TOTP is the second factor for everything
 * that is not a passkey: it hardens the password login without demanding
 * WebAuthn hardware.
 */
@Component({
  selector: 'app-security',
  standalone: true,
  imports: [
    FormsModule,
    SlicePipe,
    CardComponent,
    IconComponent,
    ModalComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Personal settings</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <div class="cards">
        <akd-card title="Passkeys" [padded]="false">
          <span card-actions>
            @if (supported) {
              <button
                class="akd-btn akd-btn--primary akd-btn--sm"
                type="button"
                (click)="adding.set(!adding())"
              >
                <akd-icon name="key-round" [size]="14" />
                {{ adding() ? 'Cancel' : 'Add passkey' }}
              </button>
            }
          </span>

          @if (!supported) {
            <p class="akd-muted sm pad">This browser does not support WebAuthn.</p>
          } @else if (adding()) {
            <form class="enrol pad" (ngSubmit)="enrol()">
              <div class="akd-field">
                <label class="akd-field__label" for="pk-name">Name for the new passkey</label>
                <input
                  id="pk-name"
                  name="name"
                  class="akd-input"
                  placeholder="e.g. MacBook Touch ID"
                  [(ngModel)]="name"
                  [disabled]="busy()"
                />
                <span class="akd-field__hint">
                  Enrol one per device, and name it after the device — the name is how you will know
                  which one to revoke when it is lost.
                </span>
              </div>
              <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                {{ busy() ? 'Waiting for the authenticator…' : 'Enrol' }}
              </button>
            </form>
          }

          @if (loading()) {
            <p class="akd-muted sm pad">Loading…</p>
          } @else if (passkeys().length === 0) {
            <p class="akd-muted sm pad">
              No passkeys yet — until one is enrolled, this account signs in by password alone.
            </p>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">
                Enrolled passkeys
              </caption>
              <thead>
                <tr>
                  <th scope="col">Passkey</th>
                  <th scope="col">Added</th>
                  <th scope="col">Last used</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (pk of passkeys(); track pk.uuid) {
                  <tr>
                    <td>{{ pk.name }}</td>
                    <td class="akd-muted">{{ pk.created_at | slice: 0 : 10 }}</td>
                    <td class="akd-muted">
                      {{ pk.last_used_at ? (pk.last_used_at | slice: 0 : 10) : 'never' }}
                    </td>
                    <td class="right">
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="revoke(pk)"
                        aria-label="Remove passkey"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>

        <div class="cols">
          <akd-card title="Two-factor (TOTP)">
            <span card-actions>
              @if (mfa(); as status) {
                <akd-status-badge
                  domain="resource"
                  [state]="status.enabled ? 'running' : 'stopped'"
                  [label]="status.enabled ? 'enabled' : 'disabled'"
                />
              }
            </span>

            <div class="stack">
              @if (recoveryCodes(); as codes) {
                <!-- Shown exactly once: only hashes survive server-side. -->
                <p class="sm"><strong>Save these recovery codes now.</strong></p>
                <p class="akd-muted sm">
                  Each one signs you in once if the authenticator is lost. They will never be shown
                  again.
                </p>
                <pre class="akd-secret codes">{{ recoveryText() }}</pre>
                <div class="actions">
                  <button
                    class="akd-btn akd-btn--secondary akd-btn--sm"
                    type="button"
                    (click)="copyRecoveryCodes()"
                  >
                    {{ copied() ? 'Copied' : 'Copy codes' }}
                  </button>
                  <button
                    class="akd-btn akd-btn--primary akd-btn--sm"
                    type="button"
                    (click)="recoveryCodes.set(null)"
                  >
                    I saved them
                  </button>
                </div>
              } @else if (setup(); as pending) {
                <p class="akd-muted sm">
                  Add this secret to your authenticator app (scan or type it), then confirm with the
                  six-digit code it displays. Nothing changes until the code confirms the app really
                  holds the secret.
                </p>
                <p class="sm">
                  <a [href]="pending.otpauth_uri">Open in the authenticator app</a> or enter the
                  secret manually:
                </p>
                <pre class="akd-secret codes">{{ pending.secret }}</pre>
                <form class="enrol" (ngSubmit)="confirmTotp()">
                  <div class="akd-field">
                    <label class="akd-field__label" for="totp-confirm">Six-digit code</label>
                    <input
                      id="totp-confirm"
                      name="code"
                      class="akd-input akd-input--mono"
                      inputmode="numeric"
                      autocomplete="one-time-code"
                      [(ngModel)]="totpCode"
                      [disabled]="busy()"
                    />
                  </div>
                  <button
                    class="akd-btn akd-btn--primary"
                    type="submit"
                    [disabled]="busy() || !totpCode"
                  >
                    Confirm
                  </button>
                  <button
                    class="akd-btn akd-btn--ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="cancelSetup()"
                  >
                    Cancel
                  </button>
                </form>
              } @else if (mfa(); as status) {
                @if (status.enabled) {
                  <p class="akd-muted sm">
                    Enabled since {{ status.confirmed_at ?? '' | slice: 0 : 10 }}. Signing in by
                    password asks for a code from your authenticator app; passkey sign-ins skip TOTP
                    — a passkey is already a phishing-resistant second factor. Disabling requires a
                    valid code.
                  </p>
                  <div>
                    <span class="akd-badge akd-badge--mono">
                      {{ status.recovery_codes_remaining }} recovery code(s) left
                    </span>
                  </div>
                  <div class="enrol">
                    <div class="akd-field">
                      <label class="akd-field__label" for="totp-manage">
                        {{ useRecoveryToDisable ? 'Recovery code' : 'Current six-digit code' }}
                      </label>
                      <input
                        id="totp-manage"
                        name="code"
                        class="akd-input akd-input--mono"
                        [attr.inputmode]="useRecoveryToDisable ? 'text' : 'numeric'"
                        autocomplete="one-time-code"
                        [(ngModel)]="totpCode"
                        [disabled]="busy()"
                      />
                    </div>
                    @if (!useRecoveryToDisable) {
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm"
                        type="button"
                        [disabled]="busy() || !totpCode"
                        (click)="regenerate()"
                      >
                        <akd-icon name="rotate-ccw" [size]="14" />
                        Regenerate recovery codes
                      </button>
                    }
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy() || !totpCode"
                      (click)="disableTotp()"
                    >
                      Disable
                    </button>
                  </div>
                  <label class="akd-check sm">
                    <input
                      type="checkbox"
                      name="use-recovery"
                      [(ngModel)]="useRecoveryToDisable"
                      [disabled]="busy()"
                    />
                    I lost the authenticator — use a recovery code
                  </label>
                } @else {
                  <p class="akd-muted sm">
                    Add a code from an authenticator app to every password sign-in. An attacker who
                    phishes or guesses the password still cannot get in without the code. Passkeys
                    are unaffected — they already are a second factor.
                  </p>
                  <div>
                    <button
                      class="akd-btn akd-btn--primary"
                      type="button"
                      [disabled]="busy()"
                      (click)="startSetup()"
                    >
                      Enable two-factor authentication
                    </button>
                  </div>
                }
              } @else {
                <p class="akd-muted sm">Loading…</p>
              }
            </div>
          </akd-card>

          <akd-card title="Linked accounts" [padded]="false">
            <p class="akd-muted sm pad">
              Sign in with an identity provider instead of the password. Linking is explicit and
              done from here, signed in — a provider account merely sharing your email never
              attaches itself.
            </p>

            @if (identities().length > 0) {
              <table class="akd-table">
                <caption class="sr-only">
                  Linked identities
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Provider</th>
                    <th scope="col">Email at the provider</th>
                    <th scope="col"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  @for (identity of identities(); track identity.uuid) {
                    <tr>
                      <td>{{ identity.provider }}</td>
                      <td class="akd-muted">{{ identity.email ?? '—' }}</td>
                      <td class="right">
                        <button
                          class="akd-btn akd-btn--danger akd-btn--sm"
                          type="button"
                          [disabled]="busy()"
                          (click)="unlink(identity)"
                        >
                          Unlink
                        </button>
                      </td>
                    </tr>
                  }
                </tbody>
              </table>
            }

            @if (linkableProviders().length > 0) {
              <div class="link-row pad">
                @for (p of linkableProviders(); track p.provider) {
                  <button
                    class="akd-btn akd-btn--secondary akd-btn--sm"
                    type="button"
                    [disabled]="busy()"
                    (click)="link(p.provider)"
                  >
                    Link {{ p.name }}
                  </button>
                }
              </div>
            } @else if (identities().length === 0) {
              <p class="akd-muted sm pad">No identity provider is configured on this instance.</p>
            }
          </akd-card>
        </div>

        <akd-card title="API tokens" [padded]="false">
          <button
            card-actions
            class="akd-btn akd-btn--secondary akd-btn--sm"
            type="button"
            (click)="openToken()"
          >
            <akd-icon name="plus" [size]="13" />
            New token
          </button>
          <p class="akd-muted sm pad">
            Your own access tokens for the CLI and the API, scoped to your current team — a
            colleague's are theirs to manage, and an administrator sees the whole team's under Team
            settings. The value is shown once at creation — only its hash is stored.
          </p>
          @if (tokens().length > 0) {
            <table class="akd-table">
              <caption class="sr-only">
                Your API tokens
              </caption>
              <thead>
                <tr>
                  <th scope="col">Token</th>
                  <th scope="col">Permissions</th>
                  <th scope="col">Last used</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (token of tokens(); track token.uuid) {
                  <tr>
                    <td>
                      <span class="member-id">
                        <span class="token-name akd-mono">{{ token.name }}</span>
                        <span class="sub-mono">{{ token.token_prefix }}…</span>
                      </span>
                    </td>
                    <td>
                      <span
                        class="akd-badge akd-badge--mono"
                        [class.akd-badge--danger]="token.permissions.includes('root')"
                      >
                        {{ token.permissions.join(' · ') }}
                      </span>
                    </td>
                    <td class="akd-muted">
                      {{ token.last_used_at ? (token.last_used_at | slice: 0 : 10) : 'never' }}
                    </td>
                    <td class="right">
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="revokeToken(token)"
                        aria-label="Revoke token"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </akd-card>
      </div>

      <akd-modal [open]="tokenOpen()" title="Create an API token" (closed)="tokenOpen.set(false)">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (tokenValue(); as value) {
          <div class="modal-stack">
            <span>
              Token created. The value below is shown <strong>once</strong> — only its hash is
              stored.
            </span>
            <div class="secret-line">
              <code>{{ value }}</code>
              <button
                class="akd-iconbtn akd-iconbtn--bordered"
                type="button"
                (click)="copyToken(value)"
                aria-label="Copy token"
              >
                <akd-icon [name]="tokenCopied() ? 'check' : 'copy'" [size]="15" />
              </button>
            </div>
          </div>
        } @else {
          <form id="token-form" class="modal-stack" (ngSubmit)="createToken()">
            <div class="akd-field">
              <label class="akd-field__label" for="tok-name">Name</label>
              <input
                id="tok-name"
                name="name"
                class="akd-input akd-input--mono"
                placeholder="e.g. laptop-cli"
                [(ngModel)]="tokenName"
                [disabled]="busy()"
                required
              />
            </div>
            <fieldset class="perms">
              <legend class="akd-field__label">Permissions</legend>
              @for (perm of permissions; track perm) {
                <label class="akd-check">
                  <input
                    type="checkbox"
                    [name]="'perm-' + perm"
                    [(ngModel)]="tokenPerms[perm]"
                    [disabled]="busy()"
                  />
                  <span class="akd-mono">{{ perm }}</span>
                </label>
              }
            </fieldset>
          </form>
        }
        <div modal-footer>
          @if (tokenValue()) {
            <button class="akd-btn akd-btn--ghost" type="button" (click)="tokenOpen.set(false)">
              Close
            </button>
          } @else {
            <button
              class="akd-btn akd-btn--ghost"
              type="button"
              (click)="tokenOpen.set(false)"
              [disabled]="busy()"
            >
              Cancel
            </button>
            <button
              class="akd-btn akd-btn--primary"
              type="submit"
              form="token-form"
              [disabled]="busy() || !tokenName.trim()"
            >
              <akd-icon name="key" [size]="15" />
              {{ busy() ? 'Creating…' : 'Create token' }}
            </button>
          }
        </div>
      </akd-modal>
    </div>
  `,
  styles: [
    `
      .cards {
        display: grid;
        gap: var(--space-4);
      }
      .cols {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: var(--space-4);
        align-items: start;
      }
      @media (max-width: 900px) {
        .cols {
          grid-template-columns: 1fr;
        }
      }
      .stack {
        display: grid;
        gap: var(--space-3);
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .enrol {
        display: flex;
        align-items: end;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .enrol .akd-field {
        flex: 1;
        min-width: 240px;
      }
      .codes {
        margin: 0;
        overflow-x: auto;
      }
      .sm {
        font-size: var(--text-sm);
        margin: 0;
      }
      .actions,
      .link-row {
        display: flex;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .right {
        text-align: right;
      }
      .member-id {
        display: grid;
      }
      .token-name {
        font-weight: var(--weight-medium);
      }
      .sub-mono {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .modal-stack {
        display: grid;
        gap: var(--space-4);
      }
      .secret-line {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        background: var(--bg-inset);
        border: 1px dashed var(--accent-border);
        border-radius: var(--radius-2);
        padding: var(--space-2) var(--space-3);
      }
      .secret-line code {
        flex: 1;
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        color: var(--text-1);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .perms {
        display: grid;
        gap: var(--space-2);
        margin: 0;
        padding: 0;
        border: 0;
      }
      .perms legend {
        padding: 0;
        margin-bottom: var(--space-1);
      }
    `,
  ],
})
export class SecurityComponent {
  private readonly api = inject(ApiService);
  private readonly route = inject(ActivatedRoute);

  protected readonly supported = this.api.passkeysSupported();
  protected readonly passkeys = signal<Passkey[]>([]);
  protected readonly identities = signal<LinkedIdentity[]>([]);
  protected readonly oauthButtons = signal<OauthProviderButton[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly adding = signal(false);
  protected name = '';

  // API tokens (scoped to the current team, minted per operator).
  protected readonly permissions = PERMISSIONS;
  protected readonly tokens = signal<ApiToken[]>([]);
  protected readonly tokenOpen = signal(false);
  protected readonly tokenValue = signal<string | null>(null);
  protected readonly tokenCopied = signal(false);
  protected tokenName = '';
  protected tokenPerms: Record<ApiTokenPermission, boolean> = {
    read: true,
    'read:sensitive': false,
    write: false,
    deploy: false,
    root: false,
  };
  private readonly teamUuid = this.api.currentUser()?.teamUuid ?? null;

  protected readonly mfa = signal<MfaStatus | null>(null);
  // A setup in progress: the secret is on screen, waiting for its first code.
  protected readonly setup = signal<TotpSetup | null>(null);
  // Fresh recovery codes, displayed exactly once and then gone for good.
  protected readonly recoveryCodes = signal<string[] | null>(null);
  protected readonly copied = signal(false);
  protected totpCode = '';
  protected useRecoveryToDisable = false;

  constructor() {
    void this.load();
    // The link callback bounces back here with a code: name what happened.
    const params = this.route.snapshot.queryParamMap;
    if (params.get('linked')) {
      // Nothing to do — the reload below already shows the new identity.
    } else if (params.get('error') === 'identity_taken') {
      this.error.set('This provider account is already linked to another user.');
    } else if (params.get('error')) {
      this.error.set('Linking through the identity provider failed — try again.');
    }
  }

  private async load(): Promise<void> {
    try {
      const [passkeys, mfa, identities, providers] = await Promise.all([
        this.api.listPasskeys(),
        this.api.mfaStatus(),
        this.api.listIdentities(),
        this.api.oauthProviders(),
      ]);
      this.passkeys.set(passkeys);
      this.mfa.set(mfa);
      this.identities.set(identities);
      this.oauthButtons.set(providers);
      await this.loadTokens();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private async loadTokens(): Promise<void> {
    if (!this.teamUuid) return;
    // `mine` is the server default, said out loud here: this page is the
    // personal one, and it must never enumerate a colleague's credentials.
    const page = await this.api
      .client()
      .listApiTokens(this.teamUuid, { limit: 100, scope: 'mine' });
    this.tokens.set(page.data);
  }

  protected openToken(): void {
    this.tokenValue.set(null);
    this.tokenCopied.set(false);
    this.tokenOpen.set(true);
  }

  protected async createToken(): Promise<void> {
    if (!this.teamUuid || !this.tokenName.trim()) return;
    const permissions = PERMISSIONS.filter((p) => this.tokenPerms[p]);
    if (permissions.length === 0) {
      this.error.set('Select at least one permission.');
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.tokenValue.set(null);
    try {
      const created = await this.api.client().createApiToken(this.teamUuid, {
        name: this.tokenName.trim(),
        permissions,
        ip_allowlist: [],
      });
      this.tokenValue.set(created.token);
      this.tokenName = '';
      await this.loadTokens();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revokeToken(token: ApiToken): Promise<void> {
    if (!this.teamUuid) return;
    if (
      !confirm(
        `Revoke the token "${token.name}"? Every script or CLI session using it stops working immediately.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().revokeApiToken(this.teamUuid, token.uuid);
      await this.loadTokens();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async copyToken(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      this.tokenCopied.set(true);
      setTimeout(() => this.tokenCopied.set(false), 2000);
    } catch {
      // Clipboard may be unavailable — the secret stays selectable in the box.
    }
  }

  /** Enabled providers the account is NOT yet linked to. */
  protected linkableProviders(): OauthProviderButton[] {
    const linked = new Set(this.identities().map((i) => i.provider));
    return this.oauthButtons().filter((p) => !linked.has(p.provider));
  }

  protected async link(provider: string): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.startOauth(provider, 'link'); // navigates away on success
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }

  protected async unlink(identity: LinkedIdentity): Promise<void> {
    if (
      !confirm(
        `Unlink ${identity.provider}? Signing in through it will no longer reach this account.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.deleteIdentity(identity.uuid);
      this.identities.set(await this.api.listIdentities());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async enrol(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.registerPasskey(this.name.trim() || 'passkey');
      this.name = '';
      this.adding.set(false);
      await this.load();
    } catch (err) {
      this.error.set(
        err instanceof DOMException
          ? 'Passkey enrolment was cancelled or failed.'
          : ApiService.describe(err),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected async revoke(pk: Passkey): Promise<void> {
    // A revoked passkey cannot sign in again — worth one explicit confirmation,
    // especially when it is the last one and the password becomes the only way
    // back in.
    if (!confirm(`Revoke the passkey "${pk.name}"? A device holding it will no longer sign in.`)) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.deletePasskey(pk.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- TOTP -----------------------------------------------------------------

  protected recoveryText(): string {
    return (this.recoveryCodes() ?? []).join('\n');
  }

  protected async startSetup(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      this.setup.set(await this.api.setupTotp());
      this.totpCode = '';
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected cancelSetup(): void {
    // The unconfirmed factor guards nothing; the next setup simply replaces it.
    this.setup.set(null);
    this.totpCode = '';
    this.error.set(null);
  }

  protected async confirmTotp(): Promise<void> {
    if (this.busy() || !this.totpCode) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const codes = await this.api.confirmTotp(this.totpCode.trim());
      this.setup.set(null);
      this.totpCode = '';
      this.recoveryCodes.set(codes);
      this.copied.set(false);
      this.mfa.set(await this.api.mfaStatus());
      // Under forced enrollment, the session was blocked until now: refresh the
      // current user so the mfa_enrollment_required gate lifts and the rest of
      // the app becomes reachable again.
      await this.api.restore();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async disableTotp(): Promise<void> {
    if (this.busy() || !this.totpCode) return;
    if (!confirm('Disable two-factor authentication? The password alone will sign in again.')) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      const code = this.totpCode.trim();
      if (this.useRecoveryToDisable) {
        await this.api.disableTotp('', code);
      } else {
        await this.api.disableTotp(code);
      }
      this.totpCode = '';
      this.useRecoveryToDisable = false;
      this.mfa.set(await this.api.mfaStatus());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async regenerate(): Promise<void> {
    if (this.busy() || !this.totpCode) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const codes = await this.api.regenerateRecoveryCodes(this.totpCode.trim());
      this.totpCode = '';
      this.recoveryCodes.set(codes);
      this.copied.set(false);
      this.mfa.set(await this.api.mfaStatus());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async copyRecoveryCodes(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.recoveryText());
      this.copied.set(true);
    } catch {
      // Clipboard can be denied; the codes stay selectable on screen.
    }
  }
}
