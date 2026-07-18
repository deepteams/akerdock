import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type S3Storage = components['schemas']['S3Storage'];

@Component({
  selector: 'app-s3-storages',
  standalone: true,
  imports: [FormsModule, CardComponent, EmptyStateComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>S3 storages</h2>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      <akd-card [title]="editing() ? 'Edit storage' : 'Add a storage'" class="create">
        <form class="fields" (ngSubmit)="save()">
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="s3-name">Name</label>
              <input
                id="s3-name"
                name="name"
                class="akd-input akd-input--mono"
                [(ngModel)]="name"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="s3-endpoint">Endpoint</label>
              <input
                id="s3-endpoint"
                name="endpoint"
                class="akd-input akd-input--mono"
                placeholder="https://s3.eu-west-3.amazonaws.com"
                [(ngModel)]="endpoint"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="s3-bucket">Bucket</label>
              <input
                id="s3-bucket"
                name="bucket"
                class="akd-input akd-input--mono"
                [(ngModel)]="bucket"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="s3-region">Region (optional)</label>
              <input
                id="s3-region"
                name="region"
                class="akd-input akd-input--mono"
                placeholder="eu-west-3"
                [(ngModel)]="region"
                [disabled]="busy()"
              />
            </div>
          </div>
          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="s3-access">Access key</label>
              <input
                id="s3-access"
                name="access_key"
                class="akd-input akd-input--mono"
                autocomplete="off"
                [placeholder]="editing() ? 'leave blank to keep' : ''"
                [(ngModel)]="accessKey"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="s3-secret">Secret key</label>
              <input
                id="s3-secret"
                name="secret_key"
                type="password"
                class="akd-input"
                autocomplete="new-password"
                [placeholder]="editing() ? 'leave blank to keep' : ''"
                [(ngModel)]="secretKey"
                [disabled]="busy()"
              />
            </div>
          </div>
          <p class="form-hint">
            The keys are write-only: encrypted at rest and never returned by the API.
          </p>
          <div class="actions">
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
              {{ editing() ? 'Save changes' : 'Add storage' }}
            </button>
            @if (editing()) {
              <button class="akd-btn akd-btn--ghost" type="button" (click)="cancelEdit()">
                Cancel
              </button>
            }
          </div>
        </form>
      </akd-card>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (storages().length === 0) {
        <akd-empty-state
          icon="archive"
          title="No S3 storages yet"
          message="Backups need one to leave the server."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              S3-compatible storages of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Endpoint</th>
                <th scope="col">Bucket</th>
                <th scope="col">Status</th>
                <th scope="col">Last check</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (storage of storages(); track storage.uuid) {
                <tr>
                  <td class="akd-mono">{{ storage.name }}</td>
                  <td class="akd-mono akd-muted">{{ storage.endpoint }}</td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ storage.bucket }}</span>
                  </td>
                  <td>
                    <akd-status-badge
                      domain="resource"
                      [state]="
                        storage.is_usable
                          ? 'ready'
                          : storage.last_check_error
                            ? 'unhealthy'
                            : 'unknown'
                      "
                      [label]="
                        storage.is_usable
                          ? 'usable'
                          : storage.last_check_error
                            ? 'failing'
                            : 'unverified'
                      "
                    />
                  </td>
                  <td class="akd-muted">{{ storage.last_check_error ?? '—' }}</td>
                  <td class="right">
                    <span class="row-actions">
                      <button
                        class="akd-btn akd-btn--secondary akd-btn--sm"
                        type="button"
                        [disabled]="validating() === storage.uuid || busy()"
                        (click)="validate(storage)"
                      >
                        <akd-icon name="refresh-cw" [size]="13" />
                        {{ validating() === storage.uuid ? 'Validating…' : 'Validate' }}
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="edit(storage)"
                        aria-label="Edit storage"
                      >
                        <akd-icon name="pencil" [size]="15" />
                      </button>
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="remove(storage)"
                        aria-label="Delete storage"
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
        <p class="footnote">
          Validate does a real round-trip: it writes, reads back and deletes a probe object in the
          bucket.
        </p>
      }
    </div>
  `,
  styles: [
    `
      .create {
        margin-bottom: var(--space-5);
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .row {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
        gap: var(--space-4);
      }
      .form-hint {
        margin: 0;
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .actions {
        display: flex;
        gap: var(--space-2);
      }
      .row-actions {
        display: inline-flex;
        align-items: center;
        gap: var(--space-1);
        justify-content: flex-end;
      }
      .footnote {
        margin: var(--space-2) 0 0;
        font-size: var(--text-xs);
        color: var(--text-3);
      }
    `,
  ],
})
export class S3StoragesComponent {
  private readonly api = inject(ApiService);

  protected readonly storages = signal<S3Storage[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly validating = signal<string | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly editing = signal<S3Storage | null>(null);

  protected name = '';
  protected endpoint = '';
  protected bucket = '';
  protected region = '';
  protected accessKey = '';
  protected secretKey = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listS3Storages({ limit: 100 });
      this.storages.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected edit(storage: S3Storage): void {
    this.editing.set(storage);
    this.name = storage.name;
    this.endpoint = storage.endpoint;
    this.bucket = storage.bucket;
    this.region = storage.region ?? '';
    this.accessKey = '';
    this.secretKey = '';
  }

  protected cancelEdit(): void {
    this.editing.set(null);
    this.name = '';
    this.endpoint = '';
    this.bucket = '';
    this.region = '';
    this.accessKey = '';
    this.secretKey = '';
  }

  protected async save(): Promise<void> {
    const current = this.editing();
    if (!this.name.trim() || !this.endpoint.trim() || !this.bucket.trim()) return;
    if (!current && (!this.accessKey || !this.secretKey)) {
      this.error.set('Access key and secret key are required.');
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      if (current) {
        // The keys never come back, so blank fields mean "keep the stored
        // ones" — only send a key the operator typed anew.
        await this.api.client().updateS3Storage(current.uuid, current.version, {
          name: this.name.trim(),
          endpoint: this.endpoint.trim(),
          bucket: this.bucket.trim(),
          region: this.region.trim() || null,
          ...(this.accessKey ? { access_key: this.accessKey } : {}),
          ...(this.secretKey ? { secret_key: this.secretKey } : {}),
        });
      } else {
        await this.api.client().createS3Storage({
          name: this.name.trim(),
          endpoint: this.endpoint.trim(),
          bucket: this.bucket.trim(),
          region: this.region.trim() || null,
          access_key: this.accessKey,
          secret_key: this.secretKey,
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

  /** A real probe, not a ping: the server writes, reads and deletes an object. */
  protected async validate(storage: S3Storage): Promise<void> {
    this.validating.set(storage.uuid);
    this.error.set(null);
    try {
      const updated = await this.api.client().validateS3Storage(storage.uuid);
      this.storages.set(this.storages().map((s) => (s.uuid === updated.uuid ? updated : s)));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.validating.set(null);
    }
  }

  protected async remove(storage: S3Storage): Promise<void> {
    if (!confirm(`Delete the storage "${storage.name}"? Backup plans using it will fail.`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteS3Storage(storage.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
