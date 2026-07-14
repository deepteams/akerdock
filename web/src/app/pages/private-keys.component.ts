import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type PrivateKey = components['schemas']['PrivateKey'];

@Component({
  selector: 'app-private-keys',
  standalone: true,
  imports: [FormsModule, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Private keys</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <h2>Add a key</h2>
        <form class="form" (ngSubmit)="create()">
          <div class="akd-field">
            <label for="pk-name">Name</label>
            <input
              id="pk-name"
              name="name"
              class="akd-input"
              placeholder="e.g. deploy-key-prod"
              [(ngModel)]="name"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label for="pk-material">Private key (PEM / OpenSSH, no passphrase)</label>
            <textarea
              id="pk-material"
              name="private_key"
              class="akd-textarea"
              rows="6"
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              [(ngModel)]="material"
              [disabled]="busy()"
              required
            ></textarea>
            <p class="akd-muted hint">
              Stored encrypted. The private material is not echoed back on creation — reveal it
              later only with the read:sensitive permission.
            </p>
          </div>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy()">Add key</button>
          </div>
        </form>
      </section>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (keys().length === 0) {
        <div class="akd-empty">
          <p><strong>No private keys yet.</strong></p>
          <p>Servers are reached over SSH with a key registered here.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Private SSH keys of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Fingerprint</th>
              <th scope="col">In use</th>
              <th scope="col">Created</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (pk of keys(); track pk.uuid) {
              <tr>
                <td>{{ pk.name }}</td>
                <td class="akd-mono">{{ pk.fingerprint }}</td>
                <td class="akd-muted">{{ pk.in_use ? 'yes' : 'no' }}</td>
                <td class="akd-muted">{{ pk.created_at | slice: 0 : 10 }}</td>
                <td class="right">
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="toggleReveal(pk)"
                  >
                    {{ revealed()[pk.uuid] ? 'Hide' : 'Reveal' }}
                  </button>
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="rename(pk)"
                  >
                    Rename
                  </button>
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(pk)"
                  >
                    Delete
                  </button>
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
      }
    </div>
  `,
  styles: [
    `
      .form {
        display: grid;
        gap: var(--akd-space-3);
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
      pre.akd-secret {
        margin: 0;
        white-space: pre-wrap;
      }
      td .akd-btn-ghost,
      td .akd-btn-danger {
        margin-left: var(--akd-space-2);
      }
    `,
  ],
})
export class PrivateKeysComponent {
  private readonly api = inject(ApiService);

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
      const page = await this.api.client().listPrivateKeys({ limit: 100 });
      this.keys.set(page.data);
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
    if (!confirm(`Delete the key "${pk.name}"? Servers using it will lose SSH access.`)) return;
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
