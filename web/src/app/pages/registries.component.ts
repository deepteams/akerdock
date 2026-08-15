import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ModalComponent } from '../../ui/modal/modal.component';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type RegistryCredential = components['schemas']['RegistryCredential'];

@Component({
  selector: 'app-registries',
  standalone: true,
  imports: [FormsModule, CardComponent, EmptyStateComponent, IconComponent, ModalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>Registry credentials</h2>
        <button class="akd-btn akd-btn--primary" type="button" (click)="openCreate()">
          <akd-icon name="plus" [size]="15" />
          Add credential
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <akd-modal
        [open]="formOpen()"
        [title]="editing() ? 'Edit credential' : 'Add a credential'"
        (closed)="closeForm()"
      >
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <form id="rc-form" class="modal-stack" (ngSubmit)="save()">
          <div class="akd-field">
            <label class="akd-field__label" for="rc-name">Name</label>
            <input
              id="rc-name"
              name="name"
              class="akd-input akd-input--mono"
              placeholder="e.g. ghcr-company"
              [(ngModel)]="name"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="rc-url">Registry host</label>
            <input
              id="rc-url"
              name="registry_url"
              class="akd-input akd-input--mono"
              placeholder="e.g. ghcr.io"
              [(ngModel)]="registryUrl"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="rc-user">Username</label>
            <input
              id="rc-user"
              name="username"
              class="akd-input akd-input--mono"
              [(ngModel)]="username"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="rc-pass">Password or token</label>
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
            <span class="akd-field__hint">
              Write-only: encrypted at rest and never returned by the API — it only exists again at
              docker login time on the server.
            </span>
          </div>
        </form>
        <div modal-footer>
          <button
            class="akd-btn akd-btn--ghost"
            type="button"
            (click)="closeForm()"
            [disabled]="busy()"
          >
            Cancel
          </button>
          <button class="akd-btn akd-btn--primary" type="submit" form="rc-form" [disabled]="busy()">
            {{ busy() ? 'Saving…' : editing() ? 'Save changes' : 'Add credential' }}
          </button>
        </div>
      </akd-modal>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (credentials().length === 0) {
        <akd-empty-state
          icon="box"
          title="No registry credentials yet"
          message="Add one to pull images from a private registry."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Private registry credentials of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Registry</th>
                <th scope="col">Username</th>
                <th scope="col">In use</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (cred of credentials(); track cred.uuid) {
                <tr>
                  <td class="akd-mono">{{ cred.name }}</td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ cred.registry_url }}</span>
                  </td>
                  <td class="akd-mono akd-muted">{{ cred.username }}</td>
                  <td>
                    @if (cred.in_use) {
                      <span class="akd-badge akd-badge--accent">in use</span>
                    } @else {
                      <span class="akd-badge">unused</span>
                    }
                  </td>
                  <td class="right">
                    <span class="row-actions">
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="edit(cred)"
                        aria-label="Edit credential"
                      >
                        <akd-icon name="pencil" [size]="15" />
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="remove(cred)"
                        aria-label="Delete credential"
                      >
                        <akd-icon name="trash-2" [size]="15" />
                      </button>
                    </span>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .modal-stack {
        display: grid;
        gap: var(--space-4);
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
export class RegistriesComponent {
  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

  protected readonly credentials = signal<RegistryCredential[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly editing = signal<RegistryCredential | null>(null);
  protected readonly formOpen = signal(false);

  protected name = '';
  protected registryUrl = '';
  protected username = '';
  protected password = '';

  constructor() {
    void this.load();
  }

  protected openCreate(): void {
    this.cancelEdit();
    this.formOpen.set(true);
  }

  protected closeForm(): void {
    this.formOpen.set(false);
    this.cancelEdit();
  }

  private async load(): Promise<void> {
    try {
      const credentials = await fetchAll((cursor) =>
        this.api.client().listRegistryCredentials({ limit: 100, cursor }),
      );
      this.credentials.set(credentials);
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
    this.formOpen.set(true);
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
      this.closeForm();
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(cred: RegistryCredential): Promise<void> {
    if (
      !(await this.confirm.ask({
        title: 'Delete the credential',
        message: `Delete the credential "${cred.name}"? Pulls from ${cred.registry_url} will fail.`,
        confirmLabel: 'Delete',
      }))
    ) {
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
