import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { DrawerComponent } from '../../../ui/drawer/drawer.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type Storage = components['schemas']['PersistentStorage'];

@Component({
  selector: 'app-application-storages-tab',
  standalone: true,
  imports: [FormsModule, CardComponent, DrawerComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <div class="bar">
      <button class="akd-btn akd-btn--primary" type="button" (click)="openAdd()" [disabled]="busy()">
        <akd-icon name="plus" [size]="15" />
        Add storage
      </button>
    </div>

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (storages().length === 0) {
      <akd-empty-state
        icon="hard-drive"
        title="No persistent storage"
        message="Without one, everything the container writes disappears at the next deployment."
      />
    } @else {
      <akd-card title="Persistent storages" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            Persistent storages of this application
          </caption>
          <thead>
            <tr>
              <th scope="col">Kind</th>
              <th scope="col">Source</th>
              <th scope="col">Mount path</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (storage of storages(); track storage.uuid) {
              <tr>
                <td>
                  <span class="akd-badge akd-badge--mono">{{ storage.kind }}</span>
                  @if (storage.is_generated) {
                    <span class="akd-badge">compose</span>
                  }
                </td>
                <td class="akd-mono">
                  {{
                    storage.kind === 'volume'
                      ? (storage.docker_volume_name ?? storage.name)
                      : storage.host_path
                  }}
                </td>
                <td class="akd-mono">{{ storage.mount_path }}</td>
                <td class="right">
                  @if (storage.is_generated) {
                    <!-- A mirror of the compose file: the file is the truth,
                         deleting the row would only resurrect it next deploy. -->
                    <span class="akd-muted">declared in compose</span>
                  } @else {
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(storage)"
                    >
                      Delete
                    </button>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }

    <akd-drawer [open]="showAdd()" title="Add storage" (closed)="closeAdd()">
      <form id="storage-form" class="form" (ngSubmit)="create()">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <div class="akd-field">
          <label class="akd-field__label" for="st-kind">Kind</label>
          <div class="akd-select">
            <select id="st-kind" name="kind" class="akd-input" [(ngModel)]="kind" [disabled]="busy()">
              <option value="volume">Named volume (recommended)</option>
              <option value="bind">Bind mount (host directory)</option>
            </select>
          </div>
        </div>
        @if (kind === 'volume') {
          <div class="akd-field">
            <label class="akd-field__label" for="st-name">Volume name</label>
            <input
              id="st-name"
              name="name"
              class="akd-input"
              placeholder="e.g. data"
              [(ngModel)]="name"
              [disabled]="busy()"
            />
          </div>
        } @else {
          <div class="akd-field">
            <label class="akd-field__label" for="st-host">Host path (absolute, on the server)</label>
            <input
              id="st-host"
              name="hostPath"
              class="akd-input akd-input--mono"
              placeholder="/srv/app-data"
              [(ngModel)]="hostPath"
              [disabled]="busy()"
            />
          </div>
        }
        <div class="akd-field">
          <label class="akd-field__label" for="st-mount">Mount path (in the container)</label>
          <input
            id="st-mount"
            name="mountPath"
            class="akd-input akd-input--mono"
            placeholder="/data"
            [(ngModel)]="mountPath"
            [disabled]="busy()"
          />
        </div>
      </form>
      <div drawer-footer>
        <button class="akd-btn akd-btn--ghost" type="button" (click)="closeAdd()" [disabled]="busy()">
          Cancel
        </button>
        <button
          class="akd-btn akd-btn--primary"
          type="submit"
          form="storage-form"
          [disabled]="busy() || !valid()"
        >
          <akd-icon name="plus" [size]="15" />
          Add storage
        </button>
      </div>
    </akd-drawer>
  `,
  styles: [
    `
      .bar {
        display: flex;
        justify-content: flex-end;
        margin-bottom: var(--space-5);
      }
      .form {
        display: grid;
        gap: var(--space-3);
      }
    `,
  ],
})
export class ApplicationStoragesTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly storages = signal<Storage[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly showAdd = signal(false);

  protected kind: 'volume' | 'bind' = 'volume';
  protected name = '';
  protected hostPath = '';
  protected mountPath = '';

  protected openAdd(): void {
    this.kind = 'volume';
    this.name = '';
    this.hostPath = '';
    this.mountPath = '';
    this.error.set(null);
    this.showAdd.set(true);
  }

  protected closeAdd(): void {
    if (this.busy()) return;
    this.showAdd.set(false);
  }

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected valid(): boolean {
    if (!this.mountPath.trim()) return false;
    return this.kind === 'volume' ? !!this.name.trim() : !!this.hostPath.trim();
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const page = await this.api.client().listApplicationStorages(uuid);
      this.storages.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createApplicationStorage(this.uuid(), {
        kind: this.kind,
        name: this.kind === 'volume' ? this.name.trim() : undefined,
        host_path: this.kind === 'bind' ? this.hostPath.trim() : undefined,
        mount_path: this.mountPath.trim(),
      });
      this.name = '';
      this.hostPath = '';
      this.mountPath = '';
      this.showAdd.set(false);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(storage: Storage): Promise<void> {
    if (
      !confirm(
        `Delete the storage mounted at "${storage.mount_path}"? The container loses the mount at the next deployment; the underlying data is not wiped by this action.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteApplicationStorage(this.uuid(), storage.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
