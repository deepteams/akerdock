import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
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
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>GitHub Apps</h1>
        <button class="akd-btn" type="button" (click)="creating.set(!creating())">
          {{ creating() ? 'Cancel' : 'New GitHub App' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <p class="akd-muted">
            AkerDock generates the app on GitHub for you (manifest flow): you will be
            redirected to GitHub to confirm, then to install it. No key or secret to
            paste — GitHub sends them straight to this instance, encrypted at rest.
          </p>
          <div class="akd-field">
            <label for="gh-org">GitHub organization (empty = your personal account)</label>
            <input
              id="gh-org"
              name="organization"
              class="akd-input"
              placeholder="my-org"
              [(ngModel)]="organization"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label for="gh-api">GitHub Enterprise API URL (empty = github.com)</label>
            <input
              id="gh-api"
              name="apiUrl"
              class="akd-input akd-mono"
              placeholder="https://ghe.example.com/api/v3"
              [(ngModel)]="apiUrl"
              [disabled]="busy()"
            />
          </div>
          <div>
            <button class="akd-btn" type="submit" [disabled]="busy()">
              {{ busy() ? 'Preparing…' : 'Create on GitHub' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (apps().length === 0) {
        <div class="akd-empty">
          <p><strong>No GitHub App yet.</strong></p>
          <p class="akd-muted">
            A GitHub App gives you private repository discovery, one-click application
            creation from your repos, and auto-deploy on push — without pasting deploy
            keys or configuring webhooks by hand.
          </p>
        </div>
      } @else {
        <table class="akd-table">
          <caption class="sr-only">GitHub Apps of this team</caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">App ID</th>
              <th scope="col">Status</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            @for (app of apps(); track app.uuid) {
              <tr>
                <td>{{ app.name }}</td>
                <td class="akd-mono">{{ app.app_id ?? '—' }}</td>
                <td>
                  @if (app.is_installed) {
                    <span class="ok">installed</span>
                  } @else if (app.app_id) {
                    <span class="akd-muted">created — not installed</span>
                  } @else {
                    <span class="akd-muted">draft (finish the flow on GitHub)</span>
                  }
                </td>
                <td class="right">
                  @if (!app.is_installed && app.install_url) {
                    <a class="akd-btn" [href]="app.install_url">Install</a>
                  }
                  <button
                    class="akd-btn-ghost"
                    type="button"
                    [disabled]="busy()"
                    (click)="remove(app)"
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
      .create {
        margin-bottom: var(--akd-space-5);
        max-width: 36rem;
      }
      .right {
        text-align: right;
        display: flex;
        gap: var(--akd-space-2);
        justify-content: flex-end;
      }
      .ok {
        color: var(--akd-status-success-fg, var(--akd-text));
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
