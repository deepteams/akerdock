import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ApiError } from '../../api/client';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type ExternalEndpoint = components['schemas']['ExternalEndpoint'];

/**
 * The access request page (ADR-045 §5) — the one the CLI deep-links to when a
 * mint comes back `access_request_required`.
 *
 * Two things are being collected, and both matter. The **reason** is what makes
 * the audit trail worth having: "who was present" is worth less to an auditor
 * than "who asked for access to the production replica, for what, and for how
 * long". The **second factor** is what makes the grant mean something: without
 * it a stolen dashboard cookie would be enough to grant oneself the tunnel.
 *
 * The UI never chooses the factor. The server names the one this user must
 * present — the passkey when they have one enrolled — because offering a menu
 * would let an attacker pick the weakest.
 */
@Component({
  selector: 'app-request-endpoint-access',
  standalone: true,
  imports: [FormsModule, DatePipe, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page akd-page--narrow">
      <header class="akd-bar">
        <h2>Request access</h2>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (granted(); as grant) {
        <akd-card title="Access granted">
          <p class="granted">
            <akd-icon name="check" [size]="16" />
            You can tunnel to <strong>{{ endpoint()?.name }}</strong> until
            <strong>{{ grant.expires_at | date: 'short' }}</strong
            >.
          </p>
          <p class="akd-muted">
            Go back to your terminal — the command you ran is waiting and will connect on its own.
            Opening more tunnels within this window needs nothing further.
          </p>
        </akd-card>
      } @else if (endpoint(); as ep) {
        <akd-card [title]="ep.name">
          <p class="target akd-mono">{{ ep.host }}:{{ ep.port }}</p>
          <form class="fields" (ngSubmit)="submit()">
            <div class="akd-field">
              <label class="akd-field__label" for="reason">Why do you need access?</label>
              <input
                id="reason"
                name="reason"
                class="akd-input"
                placeholder="e.g. investigating the failed migration on ticket 412"
                [(ngModel)]="reason"
                [disabled]="busy()"
                required
              />
              <span class="akd-field__hint">
                Recorded with the grant. This is what someone reads later when they ask why
                production was reached at 3am.
              </span>
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="duration">For how long (minutes)</label>
              <input
                id="duration"
                name="duration"
                type="number"
                class="akd-input akd-input--mono"
                [(ngModel)]="durationMinutes"
                [disabled]="busy()"
                required
              />
              <span class="akd-field__hint">
                Up to {{ ep.max_grant_minutes }} minutes on this endpoint. You can extend it later —
                it just asks again.
              </span>
            </div>

            @if (needsTotp()) {
              <div class="akd-field">
                <label class="akd-field__label" for="code">Authentication code</label>
                <input
                  id="code"
                  name="code"
                  class="akd-input akd-input--mono"
                  inputmode="numeric"
                  autocomplete="one-time-code"
                  placeholder="123456"
                  [(ngModel)]="code"
                  [disabled]="busy()"
                />
                <span class="akd-field__hint">
                  From your authenticator app, or one of your recovery codes.
                </span>
              </div>
            }

            <div>
              <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                {{ busy() ? 'Requesting…' : 'Request access' }}
              </button>
            </div>
          </form>
        </akd-card>
      } @else if (!error()) {
        <p class="akd-muted">Loading…</p>
      }
    </div>
  `,
  styles: [
    `
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .target {
        margin: 0 0 var(--space-4);
        color: var(--text-muted);
      }
      .granted {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        margin: 0 0 var(--space-3);
      }
    `,
  ],
})
export class RequestEndpointAccessComponent {
  private readonly api = inject(ApiService);

  /** Route parameter, bound by withComponentInputBinding(). */
  readonly uuid = input.required<string>();

  protected readonly endpoint = signal<ExternalEndpoint | null>(null);
  protected readonly granted = signal<ExternalEndpoint['active_grant'] | null>(null);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  /** Set once the server has told us this user's factor is a TOTP. */
  protected readonly needsTotp = signal(false);

  protected reason = '';
  protected durationMinutes = 240;
  protected code = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const endpoint = await this.api.client().getExternalEndpoint(this.uuid());
      this.endpoint.set(endpoint);
      this.durationMinutes = Math.min(240, endpoint.max_grant_minutes);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /**
   * Asks for the grant, and runs the ceremony the server demands.
   *
   * The step-up is not requested up front: the API says when it is needed, we
   * satisfy it and retry once. That way someone who re-authenticated a minute
   * ago is not asked again for no reason.
   */
  protected async submit(): Promise<void> {
    if (!this.reason.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.request();
    } catch (err) {
      if (!(err instanceof ApiError) || err.code !== 'stepup_required') {
        this.error.set(ApiService.describe(err));
        this.busy.set(false);
        return;
      }
      try {
        // The refusal names the factor. A passkey runs silently; a TOTP needs
        // a code, so the field appears and the user submits again.
        if (err.message.includes('passkey')) {
          await this.api.stepUpWithPasskey();
        } else if (this.code.trim()) {
          await this.api.stepUpWithTotp(this.code.trim());
        } else {
          this.needsTotp.set(true);
          this.error.set('Enter your authentication code to confirm this request.');
          this.busy.set(false);
          return;
        }
        await this.request();
      } catch (retryErr) {
        this.error.set(ApiService.describe(retryErr));
      }
    } finally {
      this.busy.set(false);
      this.code = '';
    }
  }

  private async request(): Promise<void> {
    const grant = await this.api.client().requestExternalEndpointGrant(this.uuid(), {
      reason: this.reason.trim(),
      duration_minutes: this.durationMinutes,
    });
    this.granted.set(grant);
  }
}
