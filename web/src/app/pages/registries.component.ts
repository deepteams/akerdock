import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type RegistryCredential = components['schemas']['RegistryCredential'];

@Component({
  selector: 'app-registries',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Registry credentials</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <section class="akd-card">
        <h2>{{ editing() ? 'Edit credential' : 'Add a credential' }}</h2>
        <form class="form" (ngSubmit)="save()">
          <div class="row">
            <div class="akd-field">
              <label for="rc-name">Name</label>
              <input
                id="rc-name"
                name="name"
                class="akd-input"
                placeholder="e.g. ghcr-company"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label for="rc-url">Registry host</label>
              <input
                id="rc-url"
                name="registry_url"
                class="akd-input"
                placeholder="e.g. ghcr.io"
                [(ngModel)]="registryUrl"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label for="rc-user">Username</label>
              <input
                id="rc-user"
                name="username"
                class="akd-input"
                [(ngModel)]="username"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label for="rc-pass">Password or token</label>
              <input
                id="rc-pass"
                name="password"
                type="password"
                class="akd-input"
                autocomplete="new-password"
                [placeholder]="editing() ? 'leave blank to keep' : ''"
                [(ngModel)]="password"
                [disabled]="busy()"
              />
            </div>
          </div>
          <p class="akd-muted hint">
            The password is write-only: it is encrypted at rest and never returned by the API — it
            only exists again at docker login time on the server.
          </p>
          <div class="actions">
            <button class="akd-btn" type="submit" [disabled]="busy()">
              {{ editing() ? 'Save changes' : 'Add credential' }}
            </button>
            @if (editing()) {
              <button class="akd-btn-ghost" type="button" (click)="cancelEdit()">Cancel</button>
            }
          </div>
        </form>
      </section>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (credentials().length === 0) {
        <div class="akd-empty">
          <p><strong>No registry credentials yet.</strong></p>
          <p>Add one to pull images from a private registry.</p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">Private registry credentials of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Registry</th>
              <th scope="col">Username</th>
              <th scope="col">In use</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (cred of credentials(); track cred.uuid) {
              <tr>
                <td>{{ cred.name }}</td>
                <td class="akd-mono">{{ cred.registry_url }}</td>
                <td class="akd-muted">{{ cred.username }}</td>
                <td class="akd-muted">{{ cred.in_use ? 'yes' : 'no' }}</td>
                <td class="right">
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="edit(cred)"
                  >
                    Edit
                  </button>
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(cred)"
                  >
                    Delete
                  </button>
                </td>
              </tr>
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
      .row {
        display: flex;
        gap: var(--akd-space-3);
        flex-wrap: wrap;
      }
      .row .akd-field {
        flex: 1;
        min-width: 180px;
      }
      .hint {
        margin: 0;
        font-size: var(--akd-text-xs);
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      td .akd-btn-danger {
        margin-left: var(--akd-space-2);
      }
    `,
  ],
})
export class RegistriesComponent {
  private readonly api = inject(ApiService);

  protected readonly credentials = signal<RegistryCredential[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly editing = signal<RegistryCredential | null>(null);

  protected name = '';
  protected registryUrl = '';
  protected username = '';
  protected password = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listRegistryCredentials({ limit: 100 });
      this.credentials.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected edit(cred: RegistryCredential): void {
    this.editing.set(cred);
    this.name = cred.name;
    this.registryUrl = cred.registry_url;
    this.username = cred.username;
    this.password = '';
  }

  protected cancelEdit(): void {
    this.editing.set(null);
    this.name = '';
    this.registryUrl = '';
    this.username = '';
    this.password = '';
  }

  protected async save(): Promise<void> {
    const current = this.editing();
    if (!this.name.trim() || !this.registryUrl.trim() || !this.username.trim()) return;
    if (!current && !this.password) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      if (current) {
        // The password never comes back, so a blank field means "keep the
        // stored one" — only send it when the operator typed a new value.
        await this.api.client().updateRegistryCredential(current.uuid, current.version ?? 0, {
          name: this.name.trim(),
          registry_url: this.registryUrl.trim(),
          username: this.username.trim(),
          ...(this.password ? { password: this.password } : {}),
        });
      } else {
        await this.api.client().createRegistryCredential({
          name: this.name.trim(),
          registry_url: this.registryUrl.trim(),
          username: this.username.trim(),
          password: this.password,
        });
      }
      this.cancelEdit();
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(cred: RegistryCredential): Promise<void> {
    if (!confirm(`Delete the credential "${cred.name}"? Pulls from ${cred.registry_url} will fail.`)) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteRegistryCredential(cred.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
