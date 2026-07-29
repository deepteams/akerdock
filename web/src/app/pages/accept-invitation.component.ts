import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { ApiService, type InvitationInfo } from '../core/api.service';

/**
 * Invitation landing page (ADR-038), reached as /invitations/accept?token=….
 *
 * It serves two people. Someone already signed in only has a token to redeem.
 * Someone invited to an instance where they have no account — the common case,
 * and the whole point of an invitation — has to create one first, and this is
 * the only place where that is possible: self-service signup is closed, and
 * without an OAuth provider configured there is no other door. Sending them to
 * the sign-in form, as this page used to, was a dead end.
 *
 * The route is unguarded for that reason; the server authenticates the LINK,
 * not the visitor.
 */
@Component({
  selector: 'app-accept-invitation',
  standalone: true,
  imports: [CardComponent, FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <div class="wrap">
      <akd-card [title]="cardTitle()">
        @switch (state()) {
          @case ('loading') {
            <p class="akd-muted">Checking your invitation…</p>
          }

          @case ('signup') {
            @if (invitation(); as invite) {
              <p class="lead">
                You've been invited to join <strong>{{ invite.team_name }}</strong> as
                <span class="akd-mono">{{ invite.role }}</span
                >.
              </p>
              <form (ngSubmit)="signUp()">
                <div class="akd-field">
                  <label class="akd-field__label" for="invite-email">Email</label>
                  <!-- Fixed by the invitation: the address is what the admin
                       invited, not something to choose here. -->
                  <input
                    id="invite-email"
                    class="akd-input"
                    type="email"
                    [value]="invite.email"
                    disabled
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="invite-name">Your name</label>
                  <input
                    id="invite-name"
                    class="akd-input"
                    type="text"
                    name="name"
                    autocomplete="name"
                    [(ngModel)]="name"
                    [disabled]="busy()"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="invite-password">Password</label>
                  <input
                    id="invite-password"
                    class="akd-input"
                    type="password"
                    name="password"
                    autocomplete="new-password"
                    required
                    [(ngModel)]="password"
                    [disabled]="busy()"
                  />
                  <span class="akd-field__hint">At least 12 characters.</span>
                </div>
                @if (error(); as message) {
                  <p class="akd-error" role="alert">{{ message }}</p>
                }
                <div class="actions">
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    {{ busy() ? 'Creating your account…' : 'Create account and join' }}
                  </button>
                </div>
              </form>
            }
          }

          @case ('sign-in') {
            <p class="lead">
              @if (invitation(); as invite) {
                <strong>{{ invite.email }}</strong> already has an account here. Sign in to join
                <strong>{{ invite.team_name }}</strong
                >.
              }
            </p>
            <div class="actions">
              <button class="akd-btn akd-btn--primary" type="button" (click)="goToSignIn()">
                Sign in
              </button>
            </div>
          }

          @case ('working') {
            <p class="akd-muted">Accepting your invitation…</p>
          }

          @case ('done') {
            <p class="akd-muted">
              You've joined the team. You can now switch to it from the team switcher.
            </p>
            <div class="actions">
              <button class="akd-btn akd-btn--primary" type="button" (click)="goToApp()">
                Continue
              </button>
            </div>
          }

          @case ('error') {
            <p class="akd-error" role="alert">{{ error() }}</p>
            <div class="actions">
              <button class="akd-btn akd-btn--ghost" type="button" (click)="goToApp()">
                Back to dashboard
              </button>
            </div>
          }
        }
      </akd-card>
    </div>
  `,
  styles: [
    `
      .wrap {
        max-width: 460px;
        margin: 10vh auto 0;
        padding: 0 var(--space-4);
      }
      form {
        display: grid;
        gap: var(--space-4);
      }
      .lead {
        color: var(--text-2);
        margin-bottom: var(--space-5);
      }
      .actions {
        margin-top: var(--space-5);
        display: flex;
        justify-content: flex-end;
      }
    `,
  ],
})
export class AcceptInvitationComponent {
  private readonly api = inject(ApiService);
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);

  protected readonly state = signal<
    'loading' | 'signup' | 'sign-in' | 'working' | 'done' | 'error'
  >('loading');
  protected readonly invitation = signal<InvitationInfo | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected name = '';
  protected password = '';

  private readonly token = this.route.snapshot.queryParamMap.get('token') ?? '';

  constructor() {
    if (!this.token) {
      this.error.set('This invitation link is missing its token.');
      this.state.set('error');
    } else {
      void this.start();
    }
  }

  protected cardTitle(): string {
    return this.state() === 'signup' ? 'Create your account' : 'Join a team';
  }

  /** Signed in: redeem. Not signed in: find out whether this address can sign
   *  up here, or must sign in to an account it already has. */
  private async start(): Promise<void> {
    if (this.api.isAuthenticated() || (await this.api.restore())) {
      this.state.set('working');
      await this.accept();
      return;
    }
    try {
      const invite = await this.api.invitationInfo(this.token);
      this.invitation.set(invite);
      if (invite.account_exists || invite.password_login_disabled) {
        this.state.set('sign-in');
      } else {
        this.state.set('signup');
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.state.set('error');
    }
  }

  private async accept(): Promise<void> {
    try {
      await this.api.acceptInvitation(this.token);
      this.state.set('done');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.state.set('error');
    }
  }

  protected async signUp(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      // Creating the account joins the team in the same call — the invitation
      // is claimed server-side — so there is nothing left to redeem afterwards.
      await this.api.signUpFromInvitation(this.token, this.name, this.password);
      this.state.set('done');
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected goToSignIn(): void {
    void this.router.navigate(['/sign-in'], {
      queryParams: { returnUrl: `/invitations/accept?token=${encodeURIComponent(this.token)}` },
    });
  }

  protected goToApp(): void {
    void this.router.navigate(['/applications']);
  }
}
