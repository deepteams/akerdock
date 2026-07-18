import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type GithubApp = components['schemas']['GithubApp'];

/**
 * GitHub Apps (git-webhook-protocols §2): created by the MANIFEST FLOW — the
 * dashboard never asks for an app id, a key or a secret. It posts a manifest
 * to GitHub, GitHub sends the credentials back to the instance, and the only
 * thing left to do is install the app on an account or organization.
 */
@Component({
  selector: 'app-github-apps',
  standalone: true,
  imports: [FormsModule, CardComponent, EmptyStateComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h2>GitHub Apps</h2>
        <button class="akd-btn akd-btn--primary" type="button" (click)="creating.set(!creating())">
          <akd-icon [name]="creating() ? 'x' : 'plus'" [size]="15" />
          {{ creating() ? 'Cancel' : 'New GitHub App' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <akd-card class="create">
          <form class="fields" (ngSubmit)="create()">
            <p class="intro">
              AkerDock generates the app on GitHub for you (manifest flow): you will be redirected
              to GitHub to confirm, then to install it. No key or secret to paste — GitHub sends
              them straight to this instance, encrypted at rest.
            </p>
            <div class="akd-field">
              <label class="akd-field__label" for="gh-org">GitHub organization</label>
              <input
                id="gh-org"
                name="organization"
                class="akd-input akd-input--mono"
                placeholder="my-org"
                [(ngModel)]="organization"
                [disabled]="busy()"
              />
              <span class="akd-field__hint">Empty = your personal account.</span>
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="gh-api">GitHub Enterprise API URL</label>
              <input
                id="gh-api"
                name="apiUrl"
                class="akd-input akd-input--mono"
                placeholder="https://ghe.example.com/api/v3"
                [(ngModel)]="apiUrl"
                [disabled]="busy()"
              />
              <span class="akd-field__hint">Empty = github.com.</span>
            </div>
            <div>
              <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                <akd-icon name="external-link" [size]="15" />
                {{ busy() ? 'Preparing…' : 'Create on GitHub' }}
              </button>
            </div>
          </form>
        </akd-card>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (apps().length === 0) {
        <akd-empty-state
          icon="folder-git-2"
          title="No GitHub App yet"
          message="A GitHub App gives you private repository discovery, one-click application creation from your repos, and auto-deploy on push — without pasting deploy keys or configuring webhooks by hand."
        />
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              GitHub Apps of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">App ID</th>
                <th scope="col">Status</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (app of apps(); track app.uuid) {
                <tr>
                  <td class="akd-mono">{{ app.name }}</td>
                  <td class="akd-mono akd-muted">{{ app.app_id ?? '—' }}</td>
                  <td>
                    @if (app.is_installed) {
                      <span class="akd-badge akd-badge--ok">installed</span>
                    } @else if (app.app_id) {
                      <span class="akd-badge akd-badge--warn">created — not installed</span>
                    } @else {
                      <span class="akd-badge">draft (finish the flow on GitHub)</span>
                    }
                  </td>
                  <td class="right">
                    <span class="row-actions">
                      @if (!app.is_installed && app.install_url) {
                        <a class="akd-btn akd-btn--primary akd-btn--sm" [href]="app.install_url">
                          <akd-icon name="external-link" [size]="13" />
                          Install
                        </a>
                      }
                      <button
                        class="akd-iconbtn"
                        type="button"
                        [disabled]="busy()"
                        (click)="remove(app)"
                        aria-label="Delete GitHub App"
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
      .create {
        margin-bottom: var(--space-5);
        max-width: 640px;
      }
      .fields {
        display: grid;
        gap: var(--space-4);
      }
      .intro {
        margin: 0;
        font-size: var(--text-sm);
        color: var(--text-2);
      }
      .row-actions {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
        justify-content: flex-end;
      }
    `,
  ],
})
export class GithubAppsComponent {
  private readonly api = inject(ApiService);

  protected readonly apps = signal<GithubApp[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);

  protected organization = '';
  protected apiUrl = '';

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const page = await this.api.client().listGithubApps({ limit: 100 });
      this.apps.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  /**
   * Manifest flow step 2 (protocols §2.1): the browser posts the manifest to
   * GitHub as a FORM — a fetch cannot do it, the user has to land on GitHub's
   * confirmation page. The form is built off-DOM and submitted immediately.
   */
  protected async create(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const flow = await this.api.client().createGithubApp({
        organization: this.organization.trim() || null,
        api_url: this.apiUrl.trim() || null,
        html_url: null,
      });
      const form = document.createElement('form');
      form.method = 'POST';
      form.action = flow.target_url;
      const field = document.createElement('input');
      field.type = 'hidden';
      field.name = 'manifest';
      field.value = JSON.stringify(flow.manifest);
      form.appendChild(field);
      document.body.appendChild(form);
      form.submit();
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }

  protected async remove(app: GithubApp): Promise<void> {
    if (!app.uuid || this.busy()) return;
    if (!confirm(`Delete the GitHub App "${app.name}"? The app on GitHub itself is not removed.`)) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteGithubApp(app.uuid);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
