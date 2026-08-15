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
              Stored encrypted. Once a key enters AkerDock its private material can never be
              read back — only the public key is served.
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

      <akd-card title="Generate a key" class="create">
        <form class="fields" (ngSubmit)="generate()">
          <div class="akd-field">
            <label class="akd-field__label" for="pk-gen-name">Name</label>
            <input
              id="pk-gen-name"
              name="genName"
              class="akd-input akd-input--mono"
              placeholder="e.g. prod-cluster"
              [(ngModel)]="genName"
              [disabled]="busy()"
              required
            />
            <span class="akd-field__hint">
              The keypair (ed25519) is created inside AkerDock and stored encrypted — the
              private half never exists anywhere else. Only the public key is shown, to be
              added to the server's authorized_keys.
            </span>
          </div>
          <div>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              <akd-icon name="sparkles" [size]="15" />
              Generate
            </button>
          </div>
          @if (generated(); as pk) {
            <div class="generated" role="status">
              <p class="generated__title">
                Key "{{ pk.name }}" generated — add this public key to
                <code>~/.ssh/authorized_keys</code> on the server:
              </p>
              <pre class="akd-secret">{{ pk.public_key }}</pre>
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                (click)="copy(pk.public_key ?? '')"
              >
                <akd-icon name="copy" [size]="14" />
                Copy public key
              </button>
            </div>
          }
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
                        (click)="togglePublic(pk)"
                        [attr.aria-label]="
                          shownPublic()[pk.uuid] ? 'Hide public key' : 'Show public key'
                        "
                      >
                        <akd-icon name="key-round" [size]="15" />
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
                @if (shownPublic()[pk.uuid]) {
                  <tr>
                    <td colspan="5">
                      <pre class="akd-secret">{{ pk.public_key }}</pre>
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
        word-break: break-all;
      }
      .generated {
        border-top: 1px solid var(--border-1);
        padding-top: var(--space-3);
      }
      .generated__title {
        margin: 0 0 var(--space-2);
        color: var(--text-2);
        font-size: var(--text-sm);
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
  /** Rows whose public key line is unfolded (nothing secret about it). */
  protected readonly shownPublic = signal<Record<string, boolean>>({});
  /** The last server-generated key, shown with its authorized_keys line. */
  protected readonly generated = signal<PrivateKey | null>(null);

  protected name = '';
  protected material = '';
  protected genName = '';

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
   * ADR-075: the private half never leaves the server — the response carries
   * the public key only, shown with the authorized_keys instruction.
   */
  protected async generate(): Promise<void> {
    if (!this.genName.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const pk = await this.api.client().generatePrivateKey({ name: this.genName.trim() });
      this.generated.set(pk);
      this.genName = '';
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected togglePublic(pk: PrivateKey): void {
    this.shownPublic.set({ ...this.shownPublic(), [pk.uuid]: !this.shownPublic()[pk.uuid] });
  }

  protected async copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      this.error.set('Could not write to the clipboard.');
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
