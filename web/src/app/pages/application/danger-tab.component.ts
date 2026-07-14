import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { ApiService } from '../../core/api.service';

@Component({
  selector: 'app-application-danger-tab',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <section class="akd-card zone">
      <header class="akd-bar" style="margin-bottom: 0">
        <h2>Delete this application</h2>
      </header>
      <p class="akd-muted">
        The container is stopped and removed, the routing is dropped, and the configuration is
        deleted. Deletion runs as a job — it continues even if this page is closed.
      </p>
      <label class="check">
        <input
          type="checkbox"
          name="deleteVolumes"
          [(ngModel)]="deleteVolumes"
          [disabled]="busy()"
        />
        Also delete its volumes — the persisted data is destroyed with them
      </label>
      <div>
        <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="destroy()">
          {{ busy() ? 'Deleting…' : 'Delete application' }}
        </button>
      </div>
    </section>
  `,
  styles: [
    `
      .zone {
        max-width: 44rem;
        border-color: var(--akd-status-danger-fg);
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
    `,
  ],
})
export class ApplicationDangerTabComponent {
  readonly uuid = input.required<string>();
  readonly name = input<string>('');

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected deleteVolumes = false;

  protected async destroy(): Promise<void> {
    const label = this.name() || 'this application';
    const volumes = this.deleteVolumes
      ? ' Its volumes and the data they hold are destroyed too.'
      : ' Its volumes are kept.';
    if (!confirm(`Delete ${label}? The container and its routing are removed.${volumes}`)) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteApplication(this.uuid(), {
        delete_volumes: this.deleteVolumes,
      });
      await this.router.navigateByUrl('/applications');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }
}
