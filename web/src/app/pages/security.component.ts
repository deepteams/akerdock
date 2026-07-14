import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService, Passkey } from '../core/api.service';

/**
 * Account security: passkey enrolment and revocation.
 *
 * A passkey is phishing-resistant where a password is not — the signature
 * binds the origin. This page exists so an operator can get to the point of
 * never typing the password again.
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

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      this.passkeys.set(await this.api.listPasskeys());
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
}
