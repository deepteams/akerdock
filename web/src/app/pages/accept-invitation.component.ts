import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { ApiService } from '../core/api.service';

/**
 * Invitation acceptance (ADR-038): reached as /invitations/accept?token=…. The
 * route guard has already established the panel session (any login method,
 * returnUrl-preserved), so here we only redeem the token — the server checks the
 * invitation email matches the signed-in account.
 */
@Component({
  selector: 'app-accept-invitation',
  standalone: true,
  imports: [CardComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <div class="wrap">
      <akd-card title="Join a team">
        @switch (state()) {
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

  protected readonly state = signal<'working' | 'done' | 'error'>('working');
  protected readonly error = signal<string | null>(null);

  constructor() {
    const token = this.route.snapshot.queryParamMap.get('token');
    if (!token) {
      this.error.set('This invitation link is missing its token.');
      this.state.set('error');
    } else {
      void this.accept(token);
    }
  }

  private async accept(token: string): Promise<void> {
    try {
      await this.api.acceptInvitation(token);
      this.state.set('done');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.state.set('error');
    }
  }

  protected goToApp(): void {
    void this.router.navigate(['/applications']);
  }
}
