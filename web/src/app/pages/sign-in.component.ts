import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router } from '@angular/router';
import { ApiService, OauthProviderButton } from '../core/api.service';

/** Machine error codes the OAuth callback redirects back with, translated
 *  for humans. Raw provider output never reaches this page. */
const OAUTH_ERRORS: Record<string, string> = {
  provider_refused: 'The identity provider cancelled or refused the sign-in.',
  state_invalid: 'The sign-in attempt expired — try again.',
  account_exists:
    'An account with this email already exists. Sign in with it, then link this provider from the Security page.',
  registration_disabled: 'Registration is disabled on this instance.',
  email_unverified: 'The identity provider reported no verified email for this account.',
  oauth_failed: 'Sign-in through the identity provider failed.',
  https_required:
    'This instance expects HTTPS (a FQDN is configured), but you reached it over plain HTTP — the session cookie would be dropped. Use https://, or clear the instance FQDN.',
};

@Component({
  selector: 'app-sign-in',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <main class="wrap">
      <div class="panel">
        <h1 class="brand">Aker<span class="brand__accent">Dock</span></h1>

        @if (challenge(); as chal) {
          <form class="akd-card card" (ngSubmit)="verify()">
            <p class="hint">
              {{
                useRecovery()
                  ? 'Enter one of your recovery codes. It only works once.'
                  : 'Enter the code from your authenticator app.'
              }}
            </p>

            @if (error(); as message) {
              <p class="akd-error" role="alert">{{ message }}</p>
            }

            <div class="akd-field">
              <label class="akd-field__label" for="mfa-code">
                {{ useRecovery() ? 'Recovery code' : 'Six-digit code' }}
              </label>
              <input
                id="mfa-code"
                name="code"
                type="text"
                class="akd-input akd-input--mono"
                [attr.inputmode]="useRecovery() ? 'text' : 'numeric'"
                autocomplete="one-time-code"
                [(ngModel)]="code"
                [disabled]="busy()"
                required
                autofocus
              />
            </div>

            <button
              class="akd-btn akd-btn--primary wide"
              type="submit"
              [disabled]="busy() || !code"
            >
              {{ busy() ? 'Verifying…' : 'Verify' }}
            </button>

            <button
              class="akd-btn akd-btn--secondary wide"
              type="button"
              [disabled]="busy()"
              (click)="toggleRecovery()"
            >
              {{ useRecovery() ? 'Use the authenticator app instead' : 'Use a recovery code' }}
            </button>

            <button type="button" class="link" [disabled]="busy()" (click)="restart()">
              Back to sign-in
            </button>
          </form>
        } @else {
          <form class="akd-card card" (ngSubmit)="submit()">
            <p class="hint">Sign in to your instance.</p>

            @if (error(); as message) {
              <p class="akd-error" role="alert">{{ message }}</p>
            }

            <div class="akd-field">
              <label class="akd-field__label" for="email">Email</label>
              <input
                id="email"
                name="email"
                type="email"
                class="akd-input"
                autocomplete="username"
                [(ngModel)]="email"
                [disabled]="busy()"
                required
              />
            </div>

            <div class="akd-field">
              <label class="akd-field__label" for="password">Password</label>
              <input
                id="password"
                name="password"
                type="password"
                class="akd-input"
                autocomplete="current-password"
                [(ngModel)]="password"
                [disabled]="busy()"
                required
              />
            </div>

            <button
              class="akd-btn akd-btn--primary wide"
              type="submit"
              [disabled]="busy() || !email || !password"
            >
              {{ busy() ? 'Signing in…' : 'Sign in' }}
            </button>

            @if (passkeysAvailable || providers().length > 0) {
              <div class="divider" aria-hidden="true"><span>or</span></div>
            }
            @if (passkeysAvailable) {
              <button
                class="akd-btn akd-btn--secondary wide"
                type="button"
                [disabled]="busy()"
                (click)="passkey()"
              >
                Sign in with a passkey
              </button>
            }
            @for (p of providers(); track p.provider) {
              <button
                class="akd-btn akd-btn--secondary wide"
                type="button"
                [disabled]="busy()"
                (click)="oauth(p.provider)"
              >
                Continue with {{ p.name }}
              </button>
            }

            <p class="hint hint--small">
              The session lives in a cookie this page cannot read — so neither can an attacker who
              manages to run script in it.
            </p>
          </form>
        }
      </div>
    </main>
  `,
  styles: [
    `
      .wrap {
        display: grid;
        place-items: center;
        min-height: 100vh;
        padding: var(--space-6);
        background: var(--surface-page);
      }
      .panel {
        display: grid;
        gap: var(--space-5);
        justify-items: center;
      }
      .brand {
        margin: 0;
        font: var(--weight-bold) var(--text-2xl) var(--font-display);
        color: var(--text-1);
      }
      .brand__accent {
        color: var(--accent);
      }
      .card {
        display: grid;
        gap: var(--space-4);
        width: 380px;
        max-width: 90vw;
        padding: var(--space-6);
      }
      .wide {
        width: 100%;
      }
      .akd-error {
        margin: 0;
      }
      .hint {
        margin: 0;
        font-size: var(--text-sm);
        color: var(--text-2);
      }
      .hint--small {
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .divider {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        color: var(--text-3);
        font-size: var(--text-xs);
      }
      .divider::before,
      .divider::after {
        content: '';
        flex: 1;
        border-top: 1px solid var(--border-1);
      }
      .link {
        justify-self: center;
        padding: 0;
        font: inherit;
        font-size: var(--text-sm);
        color: var(--text-2);
        background: transparent;
        border: 0;
        cursor: pointer;
        text-decoration: underline;
      }
      .link:hover:not(:disabled) {
        color: var(--text-1);
      }
      .link:disabled {
        opacity: 0.45;
        cursor: not-allowed;
      }
    `,
  ],
})
export class SignInComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);
  private readonly route = inject(ActivatedRoute);

  protected email = '';
  protected password = '';
  protected code = '';
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  // Set when the password was right and the server now wants the TOTP code:
  // the sign-in form gives way to the code form until verified or restarted.
  protected readonly challenge = signal<string | null>(null);
  protected readonly useRecovery = signal(false);
  protected readonly passkeysAvailable = this.apiSupportsPasskeys();
  protected readonly providers = signal<OauthProviderButton[]>([]);

  constructor() {
    void this.api.oauthProviders().then((p) => this.providers.set(p));
    // An OAuth callback that could not sign in lands back here with a code.
    const err = this.route.snapshot.queryParamMap.get('error');
    if (err) this.error.set(OAUTH_ERRORS[err] ?? OAUTH_ERRORS['oauth_failed']);
  }

  private apiSupportsPasskeys(): boolean {
    return this.api.passkeysSupported();
  }

  protected async oauth(provider: string): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.startOauth(provider); // navigates away on success
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }

  protected async passkey(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.signInWithPasskey();
      await this.router.navigate(['/applications']);
    } catch (err) {
      // A cancelled prompt surfaces as NotAllowedError with a browser-worded
      // message; keep it short rather than technical.
      this.error.set(
        err instanceof DOMException
          ? 'Passkey sign-in was cancelled or failed.'
          : ApiService.describe(err),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected async submit(): Promise<void> {
    if (!this.email || !this.password || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const result = await this.api.signIn(this.email.trim(), this.password);
      if (result.mfaRequired && result.challenge) {
        // Right password, no session yet: swap to the code form. The password
        // is dropped from memory — the challenge is all step two needs.
        this.password = '';
        this.challenge.set(result.challenge);
        return;
      }
      await this.router.navigate(['/applications']);
    } catch (err) {
      // The server answers the same thing for a wrong email and a wrong
      // password, on purpose: the UI must not soften that into a hint.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async verify(): Promise<void> {
    const chal = this.challenge();
    if (!chal || !this.code || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const code = this.code.trim();
      if (this.useRecovery()) {
        await this.api.verifyMfa(chal, '', code);
      } else {
        await this.api.verifyMfa(chal, code);
      }
      await this.router.navigate(['/applications']);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.code = '';
    } finally {
      this.busy.set(false);
    }
  }

  protected toggleRecovery(): void {
    this.useRecovery.update((v) => !v);
    this.code = '';
    this.error.set(null);
  }

  protected restart(): void {
    // The abandoned challenge simply expires server-side; nothing to revoke.
    this.challenge.set(null);
    this.useRecovery.set(false);
    this.code = '';
    this.error.set(null);
  }
}
