import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { ApiService } from '../core/api.service';

/**
 * The chrome around every authenticated page: one sidebar naming every
 * capability of the control plane, one topbar naming who is signed in.
 *
 * Navigation is exhaustive on purpose — the dashboard's contract is "every
 * action the API can do, reachable from here". A capability without a nav
 * entry is a capability that silently does not exist for UI users.
 */
@Component({
  selector: 'app-shell',
  standalone: true,
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="layout">
      <nav class="sidebar" aria-label="Primary">
        <a class="brand" routerLink="/">AkerDock</a>

        <span class="section">Deploy</span>
        <a routerLink="/projects" routerLinkActive="active">Projects</a>
        <a routerLink="/applications" routerLinkActive="active">Applications</a>
        <a routerLink="/services" routerLinkActive="active">Services</a>
        <a routerLink="/databases" routerLinkActive="active">Databases</a>
        <a routerLink="/servers" routerLinkActive="active">Servers</a>

        <span class="section">Operate</span>
        <a routerLink="/jobs" routerLinkActive="active">Jobs</a>
        <a routerLink="/events" routerLinkActive="active">Events</a>
        <a routerLink="/notifications" routerLinkActive="active">Notifications</a>

        <span class="section">Resources</span>
        <a routerLink="/github-apps" routerLinkActive="active">GitHub Apps</a>
        <a routerLink="/private-keys" routerLinkActive="active">Private keys</a>
        <a routerLink="/registries" routerLinkActive="active">Registries</a>
        <a routerLink="/dns-credentials" routerLinkActive="active">DNS credentials</a>
        <a routerLink="/s3-storages" routerLinkActive="active">S3 storages</a>

        <span class="section">Instance</span>
        <a routerLink="/team" routerLinkActive="active">Team</a>
        <a routerLink="/system" routerLinkActive="active">System</a>
        <a routerLink="/security" routerLinkActive="active">Security</a>
      </nav>

      <div class="main">
        <header class="topbar">
          <span class="who">
            {{ api.currentUser()?.name }}
            <span class="akd-muted">{{ api.currentUser()?.email }}</span>
          </span>
          <button class="akd-btn-ghost" type="button" (click)="signOut()">Sign out</button>
        </header>
        <main class="content">
          <router-outlet />
        </main>
      </div>
    </div>
  `,
  styles: [
    `
      :host {
        display: block;
        min-height: 100vh;
        background: var(--akd-bg);
      }
      .layout {
        display: grid;
        grid-template-columns: 220px 1fr;
        min-height: 100vh;
      }
      .sidebar {
        display: flex;
        flex-direction: column;
        gap: var(--akd-space-05);
        padding: var(--akd-space-4);
        background: var(--akd-surface);
        border-right: 1px solid var(--akd-border);
      }
      .brand {
        margin-bottom: var(--akd-space-4);
        font-size: var(--akd-text-lg);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text);
        text-decoration: none;
      }
      .section {
        margin: var(--akd-space-3) 0 var(--akd-space-1);
        font-size: var(--akd-text-2xs);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text-muted);
        text-transform: uppercase;
        letter-spacing: 0.06em;
      }
      .sidebar a:not(.brand) {
        padding: var(--akd-space-1) var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
        border-radius: var(--akd-radius-sm);
      }
      .sidebar a:not(.brand):hover {
        background: var(--akd-surface-hover);
        color: var(--akd-text);
      }
      .sidebar a.active {
        background: var(--akd-surface-hover);
        color: var(--akd-text);
        font-weight: var(--akd-weight-medium);
      }
      .main {
        display: flex;
        flex-direction: column;
        min-width: 0;
      }
      .topbar {
        display: flex;
        align-items: center;
        justify-content: flex-end;
        gap: var(--akd-space-4);
        padding: var(--akd-space-2) var(--akd-space-6);
        border-bottom: 1px solid var(--akd-border);
      }
      .who {
        display: flex;
        gap: var(--akd-space-2);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
      .content {
        flex: 1;
        min-width: 0;
      }
    `,
  ],
})
export class ShellComponent {
  protected readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected async signOut(): Promise<void> {
    await this.api.signOut();
    await this.router.navigate(['/sign-in']);
  }
}
