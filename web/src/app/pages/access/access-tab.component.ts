import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, effect, inject, input, signal } from '@angular/core';
import { CardComponent } from '../../../ui/card/card.component';
import { IconComponent } from '../../../ui/icon/icon.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type AccessEntry = components['schemas']['AccessEntry'];

export interface AccessPage {
  data: AccessEntry[];
  tokens_included?: boolean;
}
export type AccessFetch = () => Promise<AccessPage>;

/**
 * Who can reach this resource (ADR-046 §9) — the access review, reusable on
 * every screen that has one. The parent passes a `fetch`, so applications,
 * databases, services, projects and environments share one widget and one
 * reading.
 *
 * Each row states the **path**, not just the name: "Bob" is not actionable,
 * "Bob — member on project:billing" tells the reviewer what to change. Until
 * scoped assignments exist, every scope reads `team`, which is the truth of the
 * current model and the reason this screen ships first.
 */
@Component({
  selector: 'akd-access-tab',
  standalone: true,
  imports: [DatePipe, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else {
      <akd-card title="Who can reach this">
        <p class="intro">
          Everyone holding platform permissions here, and the role they hold them through. Computed
          live — there is no stored copy to go stale.
        </p>
        <table class="akd-table">
          <thead>
            <tr>
              <th>Subject</th>
              <th>Through</th>
              <th>Scope</th>
              <th>Can</th>
            </tr>
          </thead>
          <tbody>
            @for (entry of entries(); track entry.subject_uuid ?? entry.subject_name) {
              <tr>
                <td>
                  <strong>{{ entry.subject_name }}</strong>
                  @if (entry.subject_kind === 'token') {
                    <span class="akd-badge">token</span>
                    @if (entry.token_creator_email) {
                      <span class="akd-muted"> · created by {{ entry.token_creator_email }}</span>
                    }
                    @if (entry.token_expires_at) {
                      <span class="akd-muted">
                        · expires {{ entry.token_expires_at | date: 'shortDate' }}</span
                      >
                    }
                  } @else if (entry.subject_kind === 'instance_root') {
                    <span class="akd-badge akd-badge--warn">instance root</span>
                  }
                </td>
                <td>{{ entry.role }}</td>
                <td class="akd-mono">{{ entry.scope }}</td>
                <td class="caps">
                  @for (cap of entry.capabilities; track cap) {
                    <span class="akd-badge" [class.akd-badge--warn]="cap === 'secrets'">{{
                      cap
                    }}</span>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>

        @if (tokensIncluded() === false) {
          <p class="akd-muted note">
            <akd-icon name="info" [size]="14" />
            API tokens are not shown — that needs <code>tokens:read</code>. A token holds access
            too, so this list is only part of the picture.
          </p>
        }
        <p class="akd-muted note">
          Platform permissions only: this says nothing about who can reach the deployed application
          over the network, who holds a live tunnel, or who knows a database's own credentials.
        </p>
      </akd-card>
    }
  `,
  styles: [
    `
      .intro {
        margin: 0 0 var(--space-3);
        color: var(--text-muted);
      }
      .caps {
        display: flex;
        flex-wrap: wrap;
        gap: var(--space-1);
      }
      .note {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        margin: var(--space-3) 0 0;
        font-size: var(--text-xs);
      }
    `,
  ],
})
export class AccessTabComponent {
  private readonly api = inject(ApiService);

  /** How to fetch the view — the parent knows which resource this is. */
  readonly fetch = input.required<AccessFetch>();

  protected readonly entries = signal<AccessEntry[]>([]);
  protected readonly tokensIncluded = signal<boolean | undefined>(undefined);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);

  constructor() {
    // Reload whenever the parent swaps the fetch (a different resource).
    effect(() => {
      const fetch = this.fetch();
      void this.load(fetch);
    });
  }

  private async load(fetch: AccessFetch): Promise<void> {
    this.loading.set(true);
    this.error.set(null);
    try {
      const page = await fetch();
      this.entries.set(page.data);
      this.tokensIncluded.set(page.tokens_included);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }
}
