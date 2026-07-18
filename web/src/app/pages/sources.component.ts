import { ChangeDetectionStrategy, Component, effect, inject, signal } from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';
import { GithubAppsComponent } from './github-apps.component';
import { PrivateKeysComponent } from './private-keys.component';
import { RegistriesComponent } from './registries.component';
import { DnsCredentialsComponent } from './dns-credentials.component';
import { S3StoragesComponent } from './s3-storages.component';

type SourceTab = 'github-apps' | 'private-keys' | 'registries' | 'dns' | 's3';

const TABS: { id: SourceTab; label: string }[] = [
  { id: 'github-apps', label: 'GitHub Apps' },
  { id: 'private-keys', label: 'Private keys' },
  { id: 'registries', label: 'Registries' },
  { id: 'dns', label: 'DNS credentials' },
  { id: 's3', label: 'S3 storages' },
];

/**
 * Sources gathers every credential the platform pulls FROM: git accounts
 * (GitHub Apps, deploy keys), image registries, DNS credentials and S3
 * storages — the design kit's "Sources" entry, extended so the old top-level
 * pages keep a home in the new navigation. The tab is mirrored in the query
 * string so the old routes can redirect to a precise tab and deep links
 * survive a reload.
 */
@Component({
  selector: 'app-sources',
  standalone: true,
  imports: [
    GithubAppsComponent,
    PrivateKeysComponent,
    RegistriesComponent,
    DnsCredentialsComponent,
    S3StoragesComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Sources</h1>
      </header>

      <div class="akd-tabs" role="tablist">
        @for (tab of tabs; track tab.id) {
          <button
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="active() === tab.id"
            [attr.aria-selected]="active() === tab.id"
            (click)="select(tab.id)"
          >
            {{ tab.label }}
          </button>
        }
      </div>

      <div class="pane">
        @switch (active()) {
          @case ('github-apps') {
            <app-github-apps />
          }
          @case ('private-keys') {
            <app-private-keys />
          }
          @case ('registries') {
            <app-registries />
          }
          @case ('dns') {
            <app-dns-credentials />
          }
          @case ('s3') {
            <app-s3-storages />
          }
        }
      </div>
    </div>
  `,
  styles: [
    `
      /* The embedded pages carry their own .akd-page padding; inside a tab
         pane that padding and their h1 duplicate ours, so flatten both. */
      .pane ::ng-deep .akd-page {
        padding: 0;
      }
      .pane ::ng-deep .akd-bar h1 {
        font: var(--weight-semibold) var(--text-lg) var(--font-display);
      }
    `,
  ],
})
export class SourcesComponent {
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);

  protected readonly tabs = TABS;
  protected readonly active = signal<SourceTab>('github-apps');

  constructor() {
    const requested = this.route.snapshot.queryParamMap.get('tab') as SourceTab | null;
    if (requested && TABS.some((t) => t.id === requested)) this.active.set(requested);
    effect(() => {
      void this.router.navigate([], {
        queryParams: { tab: this.active() },
        replaceUrl: true,
      });
    });
  }

  protected select(tab: SourceTab): void {
    this.active.set(tab);
  }
}
