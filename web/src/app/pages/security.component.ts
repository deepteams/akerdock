import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService, MfaStatus, Passkey, TotpSetup } from '../core/api.service';

/**
 * Account security: passkey enrolment and revocation, and TOTP two-factor
 * authentication.
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
  imports: [FormsModule, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Security</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <header class="akd-bar" style="margin-bottom: 0">
          <h2>Passkeys</h2>
        </header>
        <p class="akd-muted">
          A passkey signs a challenge bound to this origin: a look-alike domain gets nothing to
          replay. Enrol one per device, and name it after the device — the name is how you will
          know which one to revoke when it is lost.
        </p>

        @if (!supported) {
          <p class="akd-muted">This browser does not support WebAuthn.</p>
        } @else {
          <form class="enrol" (ngSubmit)="enrol()">
            <div class="akd-field">
              <label for="pk-name">Name for the new passkey</label>
              <input
                id="pk-name"
                name="name"
                class="akd-input"
                placeholder="e.g. MacBook Touch ID"
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
            <button class="akd-btn" type="submit" [disabled]="busy()">
              {{ busy() ? 'Waiting for the authenticator…' : 'Add a passkey' }}
            </button>
          </form>
        }

        @if (loading()) {
          <p class="akd-muted">Loading…</p>
        } @else if (passkeys().length === 0) {
          <div class="akd-empty">
            <p><strong>No passkeys yet.</strong></p>
            <p>Until one is enrolled, this account signs in by password alone.</p>
          </div>
        } @else {
          <table class="akd-table">
            <caption class="sr-only">Enrolled passkeys</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Created</th>
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
                      class="akd-btn-danger"
                      type="button"
                      [disabled]="busy()"
                      (click)="revoke(pk)"
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </section>

      <section class="akd-card">
        <header class="akd-bar" style="margin-bottom: 0">
          <h2>Two-factor authentication</h2>
        </header>

        @if (recoveryCodes(); as codes) {
          <!-- Shown exactly once: only hashes survive server-side. -->
          <div class="akd-empty">
            <p><strong>Save these recovery codes now.</strong></p>
            <p>
              Each one signs you in once if the authenticator is lost. They will never be shown
              again.
            </p>
            <pre class="codes">{{ recoveryText() }}</pre>
            <button class="akd-btn" type="button" (click)="copyRecoveryCodes()">
              {{ copied() ? 'Copied' : 'Copy codes' }}
            </button>
            <button class="akd-btn" type="button" (click)="recoveryCodes.set(null)">
              I saved them
            </button>
          </div>
        } @else if (setup(); as pending) {
          <p class="akd-muted">
            Add this secret to your authenticator app (scan or type it), then confirm with the
            six-digit code it displays. Nothing changes until the code confirms the app really
            holds the secret.
          </p>
          <p>
            <a [href]="pending.otpauth_uri">Open in the authenticator app</a> or enter the secret
            manually:
          </p>
          <pre class="codes">{{ pending.secret }}</pre>
          <form class="enrol" (ngSubmit)="confirmTotp()">
            <div class="akd-field">
              <label for="totp-confirm">Six-digit code</label>
              <input
                id="totp-confirm"
                name="code"
                class="akd-input"
                inputmode="numeric"
                autocomplete="one-time-code"
                [(ngModel)]="totpCode"
                [disabled]="busy()"
              />
            </div>
            <button class="akd-btn" type="submit" [disabled]="busy() || !totpCode">Confirm</button>
            <button class="akd-btn" type="button" [disabled]="busy()" (click)="cancelSetup()">
              Cancel
            </button>
          </form>
        } @else if (mfa(); as status) {
          @if (status.enabled) {
            <p class="akd-muted">
              Enabled since {{ (status.confirmed_at ?? '') | slice: 0 : 10 }} —
              {{ status.recovery_codes_remaining }} recovery code(s) left. Signing in by password
              asks for a code from your authenticator app; passkeys are unaffected (they already
              are a second factor).
            </p>
            <div class="enrol">
              <div class="akd-field">
                <label for="totp-manage">
                  {{ useRecoveryToDisable ? 'Recovery code' : 'Current six-digit code' }}
                </label>
                <input
                  id="totp-manage"
                  name="code"
                  class="akd-input"
                  [attr.inputmode]="useRecoveryToDisable ? 'text' : 'numeric'"
                  autocomplete="one-time-code"
                  [(ngModel)]="totpCode"
                  [disabled]="busy()"
                />
              </div>
              @if (!useRecoveryToDisable) {
                <button
                  class="akd-btn"
                  type="button"
                  [disabled]="busy() || !totpCode"
                  (click)="regenerate()"
                >
                  New recovery codes
                </button>
              }
              <button
                class="akd-btn-danger"
                type="button"
                [disabled]="busy() || !totpCode"
                (click)="disableTotp()"
              >
                Disable 2FA
              </button>
            </div>
            <label class="akd-muted recovery-toggle">
              <input
                type="checkbox"
                name="use-recovery"
                [(ngModel)]="useRecoveryToDisable"
                [disabled]="busy()"
              />
              I lost the authenticator — use a recovery code
            </label>
          } @else {
            <p class="akd-muted">
              Add a code from an authenticator app to every password sign-in. An attacker who
              phishes or guesses the password still cannot get in without the code.
            </p>
            <button class="akd-btn" type="button" [disabled]="busy()" (click)="startSetup()">
              Enable two-factor authentication
            </button>
          }
        } @else {
          <p class="akd-muted">Loading…</p>
        }
      </section>
    </div>
  `,
  styles: [
    `
      .enrol {
        display: flex;
        align-items: end;
        gap: var(--akd-space-3);
        flex-wrap: wrap;
      }
      .enrol .akd-field {
        flex: 1;
        min-width: 240px;
      }
      .codes {
        padding: var(--akd-space-3);
        font-family: var(--akd-font-mono);
        font-size: var(--akd-text-sm);
        background: var(--akd-bg);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-sm);
        user-select: all;
        overflow-x: auto;
      }
      .recovery-toggle {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
      }
    `,
  ],
})
export class SecurityComponent {
  private readonly api = inject(ApiService);

  protected readonly supported = this.api.passkeysSupported();
  protected readonly passkeys = signal<Passkey[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected name = '';

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
  }

  private async load(): Promise<void> {
    try {
      const [passkeys, mfa] = await Promise.all([this.api.listPasskeys(), this.api.mfaStatus()]);
      this.passkeys.set(passkeys);
      this.mfa.set(mfa);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async enrol(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.registerPasskey(this.name.trim() || 'passkey');
      this.name = '';
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
