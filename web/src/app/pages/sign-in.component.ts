import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { ApiService } from '../core/api.service';

@Component({
  selector: 'app-sign-in',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <main class="wrap">
      <form class="card" (ngSubmit)="submit()">
        <h1>AkerDock</h1>
        <p class="hint">Sign in to your instance.</p>

        @if (error(); as message) {
          <p class="error" role="alert">{{ message }}</p>
        }

        <label for="email">Email</label>
        <input
          id="email"
          name="email"
          type="email"
          autocomplete="username"
          [(ngModel)]="email"
          [disabled]="busy()"
          required
        />

        <label for="password">Password</label>
        <input
          id="password"
          name="password"
          type="password"
          autocomplete="current-password"
          [(ngModel)]="password"
          [disabled]="busy()"
          required
        />

        <button type="submit" [disabled]="busy() || !email || !password">
          {{ busy() ? 'Signing in…' : 'Sign in' }}
        </button>

        @if (passkeysAvailable) {
          <div class="divider" aria-hidden="true"><span>or</span></div>
          <button type="button" class="passkey" [disabled]="busy()" (click)="passkey()">
            Sign in with a passkey
          </button>
        }

        <p class="hint small">
          The session lives in a cookie this page cannot read — so neither can an attacker who
          manages to run script in it.
        </p>
      </form>
    </main>
  `,
  styles: [
    `
      .wrap {
        display: grid;
        place-items: center;
        min-height: 100vh;
        background: var(--akd-bg);
      }
      .card {
        display: grid;
        gap: var(--akd-space-3);
        width: 380px;
        max-width: 90vw;
        padding: var(--akd-space-6);
        background: var(--akd-surface);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
      }
      h1 {
        margin: 0;
        font-size: var(--akd-text-xl);
        color: var(--akd-text);
      }
      label {
        font-size: var(--akd-text-sm);
        font-weight: var(--akd-weight-medium);
        color: var(--akd-text);
      }
      input {
        padding: var(--akd-space-2) var(--akd-space-3);
        font: inherit;
        font-size: var(--akd-text-md);
        color: var(--akd-text);
        background: var(--akd-bg);
        border: 1px solid var(--akd-border-input);
        border-radius: var(--akd-radius-sm);
      }
      input:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: 1px;
      }
      button {
        padding: var(--akd-space-2) var(--akd-space-4);
        font: inherit;
        font-weight: var(--akd-weight-medium);
        color: var(--akd-on-accent);
        background: var(--akd-accent);
        border: 0;
        border-radius: var(--akd-radius-sm);
        cursor: pointer;
      }
      button:hover:not(:disabled) {
        background: var(--akd-accent-hover);
      }
      button:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
      button:focus-visible {
        outline: 2px solid var(--akd-focus-ring);
        outline-offset: 2px;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
      }
      .hint.small {
        font-size: var(--akd-text-xs);
        color: var(--akd-text-muted);
      }
      .error {
        margin: 0;
        padding: var(--akd-space-2) var(--akd-space-3);
        font-size: var(--akd-text-sm);
        color: var(--akd-status-danger-fg);
        background: var(--akd-status-danger-bg);
        border-radius: var(--akd-radius-sm);
      }
      code {
        font-family: var(--akd-font-mono);
        font-size: var(--akd-text-xs);
      }
      .divider {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        color: var(--akd-text-muted);
        font-size: var(--akd-text-xs);
      }
      .divider::before,
      .divider::after {
        content: '';
        flex: 1;
        border-top: 1px solid var(--akd-border);
      }
      .passkey {
        padding: var(--akd-space-2) var(--akd-space-4);
        font: inherit;
        font-weight: var(--akd-weight-medium);
        color: var(--akd-text);
        background: transparent;
        border: 1px solid var(--akd-border-input);
        border-radius: var(--akd-radius-sm);
        cursor: pointer;
      }
      .passkey:hover:not(:disabled) {
        background: var(--akd-surface-hover);
      }
      .passkey:disabled {
        opacity: 0.6;
        cursor: not-allowed;
      }
    `,
  ],
})
export class SignInComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected email = '';
  protected password = '';
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly passkeysAvailable = this.apiSupportsPasskeys();

  private apiSupportsPasskeys(): boolean {
    return this.api.passkeysSupported();
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
      this.error.set(err instanceof DOMException ? 'Passkey sign-in was cancelled or failed.'
        : ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async submit(): Promise<void> {
    if (!this.email || !this.password || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.signIn(this.email.trim(), this.password);
      await this.router.navigate(['/applications']);
    } catch (err) {
      // The server answers the same thing for a wrong email and a wrong
      // password, on purpose: the UI must not soften that into a hint.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
