import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { RouterLink } from '@angular/router';
import { CardComponent } from '../../../ui/card/card.component';
import { EmptyStateComponent } from '../../../ui/empty-state/empty-state.component';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type Preview = components['schemas']['Preview'];

/**
 * PR previews (§20.4): one ephemeral instance per pull request, protected by
 * default. A fork PR deploys NOTHING until a maintainer approves it here
 * (INV-010) — that button is the approval.
 */
@Component({
  selector: 'app-application-previews-tab',
  standalone: true,
  imports: [RouterLink, CardComponent, EmptyStateComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (loading()) {
      <p class="akd-muted">Loading…</p>
    } @else if (previews().length === 0) {
      <akd-empty-state
        icon="git-branch"
        title="No preview yet"
        message="Enable previews in Settings, use a GitHub App source, and every pull request gets its own protected instance — destroyed on merge, close or TTL."
      />
    } @else {
      <akd-card title="Pull request previews" [padded]="false">
        <table class="akd-table">
          <caption class="sr-only">
            PR previews of this application
          </caption>
          <thead>
            <tr>
              <th scope="col">PR</th>
              <th scope="col">Branch</th>
              <th scope="col">Status</th>
              <th scope="col">URL</th>
              <th scope="col" class="right"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (p of previews(); track p.uuid) {
              <tr>
                <td>
                  <!-- The PR badge opens the preview's own page: logs,
                       storages, preview variables and its danger zone. -->
                  <a
                    class="akd-badge akd-badge--mono"
                    [routerLink]="['/applications', uuid(), 'previews', p.uuid]"
                    >#{{ p.pr_id }}</a
                  >
                </td>
                <td class="akd-mono">
                  {{ p.source_branch ?? '—' }}
                  @if (p.is_fork) {
                    <span class="akd-muted">(fork)</span>
                  }
                </td>
                <td>{{ p.status }}</td>
                <td>
                  @if (p.fqdn) {
                    <a class="akd-mono" [href]="'https://' + p.fqdn" target="_blank" rel="noopener">
                      {{ p.fqdn }}
                    </a>
                  } @else {
                    <span class="akd-muted">—</span>
                  }
                </td>
                <td class="right">
                  <a
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    [routerLink]="['/applications', uuid(), 'previews', p.uuid]"
                  >
                    Details
                  </a>
                  @if (p.is_fork && !p.fork_approved && p.status !== 'destroyed') {
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="approve(p)"
                    >
                      Approve fork
                    </button>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      </akd-card>
    }
  `,
})
export class ApplicationPreviewsTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly previews = signal<Preview[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  private async load(uuid: string): Promise<void> {
    try {
      const page = await this.api.client().listApplicationPreviews(uuid);
      this.previews.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async approve(preview: Preview): Promise<void> {
    if (!preview.uuid || this.busy()) return;
    if (
      !confirm(
        `Approve the preview of fork PR #${preview.pr_id}? Its code will be built on this server — no secret is ever injected.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().approvePreviewFork(this.uuid(), preview.uuid);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
