import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type PrivateKey = components['schemas']['PrivateKey'];

@Component({
  selector: 'app-private-keys',
  standalone: true,
  imports: [FormsModule, SlicePipe, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>Private keys</h2>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <akd-card title="Add a key" class="create">
        <form class="fields" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="pk-name">Name</label>
            <input
              id="pk-name"
              name="name"
              class="akd-input akd-input--mono"
              placeholder="e.g. deploy-key-prod"
              [(ngModel)]="name"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="pk-material">
              Private key (PEM / OpenSSH, no passphrase)
            </label>
            <textarea
              id="pk-material"
              name="private_key"
              class="akd-input akd-input--mono"
              rows="6"
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              [(ngModel)]="material"
              [disabled]="busy()"
              required
            ></textarea>
            <span class="akd-field__hint">
              Stored encrypted. The private material is not echoed back on creation — reveal it
              later only with the read:sensitive permission.
            </span>
          </div>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              <akd-icon name="plus" [size]="15" />
              Add key
            </button>
          </div>
        </form>
      </akd-card>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (keys().length === 0) {
        <akd-empty-state
          icon="key-round"
          title="No private keys yet"
          message="Servers are reached over SSH with a key registered here."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Private SSH keys of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Fingerprint</th>
                <th scope="col">In use</th>
                <th scope="col">Created</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (pk of keys(); track pk.uuid) {
                <tr>
                  <td class="akd-mono">{{ pk.name }}</td>
                  <td class="akd-mono akd-muted">{{ pk.fingerprint }}</td>
                  <td>
                    @if (pk.in_use) {
                      <span class="akd-badge akd-badge--accent">in use</span>
                    } @else {
                      <span class="akd-badge">unused</span>
                    }
                  </td>
                  <td class="akd-muted">{{ pk.created_at | slice: 0 : 10 }}</td>
                  <td class="right">
                    <span class="row-actions">
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="toggleReveal(pk)"
                        [attr.aria-label]="
                          revealed()[pk.uuid] ? 'Hide key material' : 'Reveal key material'
                        "
                      >
                        <akd-icon [name]="revealed()[pk.uuid] ? 'eye-off' : 'eye'" [size]="15" />
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="rename(pk)"
                        aria-label="Rename key"
                      >
                        <akd-icon name="pencil" [size]="15" />
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="remove(pk)"
                        aria-label="Delete key"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </span>
                  </td>
                </tr>
                @if (revealed()[pk.uuid]; as secret) {
                  <tr>
                    <td colspan="5">
                      <pre class="akd-secret">{{ secret }}</pre>
                    </td>
                  </tr>
                }
              }
            </tbody>
          </table>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--space-5);
        max-width: 640px;
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      pre.akd-secret {
        margin: var(--space-2) 0;
        white-space: pre-wrap;
      }
      .row-actions {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
        justify-content: flex-end;
      }
    `,
  ],
})
export class PrivateKeysComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly keys = signal<PrivateKey[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  /** Revealed key material, per uuid — kept only in memory, never persisted. */
  protected readonly revealed = signal<Record<string, string>>({});

  protected name = '';
  protected material = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const keys = await fetchAll((cursor) =>
        this.api.client().listPrivateKeys({ limit: 100, cursor }),
      );
      this.keys.set(keys);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (!this.name.trim() || !this.material.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createPrivateKey({
        name: this.name.trim(),
        private_key: this.material,
      });
      this.name = '';
      this.material = '';
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * The private material is only served with reveal=true AND the read:sensitive
   * permission (INV-003) — a 403 here means this session lacks it, and the
   * error says so.
   */
  protected async toggleReveal(pk: PrivateKey): Promise<void> {
    if (this.revealed()[pk.uuid]) {
      const { [pk.uuid]: _, ...rest } = this.revealed();
      this.revealed.set(rest);
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      const full = await this.api.client().getPrivateKey(pk.uuid, { reveal: true });
      if (full.private_key) {
        this.revealed.set({ ...this.revealed(), [pk.uuid]: full.private_key });
      } else {
        this.error.set('The server redacted the key material.');
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async rename(pk: PrivateKey): Promise<void> {
    const name = prompt('New name for this key', pk.name)?.trim();
    if (!name || name === pk.name) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updatePrivateKey(pk.uuid, pk.version, { name });
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(pk: PrivateKey): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the key',
        message: `Delete the key "${pk.name}"? Servers using it will lose SSH access.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deletePrivateKey(pk.uuid);
      await this.load();
    } catch (err) {
      // A 409 means the key is still referenced by a server or application —
      // the message names the dependency.
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
