import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { NavigationEnd } from '@angular/router';
import { filter, map, startWith } from 'rxjs';
import { ApiService, MyTeam } from '../core/api.service';
import { IconComponent } from '../../ui/icon/icon.component';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}
interface NavSection {
  title: string;
  items: NavItem[];
}

/**
 * The chrome around every authenticated page: one sidebar naming every
 * capability of the control plane, one topbar locating the user (breadcrumb)
 * and naming the build (version badge).
 *
 * Navigation is exhaustive on purpose — the dashboard's contract is "every
 * action the API can do, reachable from here". A capability without a nav
 * entry is a capability that silently does not exist for UI users. Resources
 * (applications, services, databases) are reached through the Projects
 * drill-down; their flat routes stay alive for deep links.
 */
@Component({
  selector: 'app-shell',
  standalone: true,
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="layout" [class.rail]="rail()">
      <aside class="sidebar">
        <a class="brand" routerLink="/">
          <img class="brand-logo" src="logo.png" alt="" aria-hidden="true" />
          @if (!rail()) {
            <span>Aker<span class="brand-accent">Dock</span></span>
          }
        </a>

        <nav class="nav akd-sidenav" aria-label="Primary">
          @for (section of sections; track section.title) {
            @if (!rail()) {
              <span class="akd-sidenav__section">{{ section.title }}</span>
            }
            @for (item of section.items; track item.path) {
              <a
                class="akd-sidenav__item"
                [routerLink]="item.path"
                routerLinkActive="akd-sidenav__item--active"
                [title]="rail() ? item.label : null"
              >
                <akd-icon [name]="item.icon" [size]="15" />
                @if (!rail()) {
                  <span>{{ item.label }}</span>
                }
              </a>
            }
          }
        </nav>

        <div class="user">
          @if (menu()) {
            <div class="user-menu" role="menu">
              @if (teams().length > 0) {
                <div class="akd-sidenav__section">Teams</div>
                @for (team of teams(); track team.uuid) {
                  <button
                    class="akd-sidenav__item"
                    role="menuitem"
                    type="button"
                    [disabled]="switching()"
                    [attr.aria-current]="team.uuid === currentTeamUuid() ? 'true' : null"
                    (click)="switchTeam(team.uuid)"
                  >
                    <akd-icon
                      [name]="team.uuid === currentTeamUuid() ? 'check' : 'users'"
                      [size]="14"
                    />
                    <span class="team-name">{{ team.name }}</span>
                    @if (team.uuid === currentTeamUuid()) {
                      <span class="current">current</span>
                    }
                  </button>
                }
                @if (switchError()) {
                  <div class="menu-error">{{ switchError() }}</div>
                }
                <div class="menu-sep"></div>
              }
              <button class="akd-sidenav__item" role="menuitem" (click)="go('/security')">
                <akd-icon name="settings" [size]="14" /><span>Personal settings</span>
              </button>
              @if (api.currentUser()?.instanceRoot) {
                <button class="akd-sidenav__item" role="menuitem" (click)="go('/system')">
                  <akd-icon name="globe" [size]="14" /><span>Global settings</span>
                </button>
              }
              <button class="akd-sidenav__item" role="menuitem" (click)="signOut()">
                <akd-icon name="log-out" [size]="14" /><span>Sign out</span>
              </button>
            </div>
          }
          <button
            class="user-btn"
            type="button"
            [class.open]="menu()"
            (click)="menu.set(!menu())"
            [attr.aria-expanded]="menu()"
          >
            <span class="avatar" aria-hidden="true">{{ initials() }}</span>
            @if (!rail()) {
              <span class="email">{{ api.currentUser()?.email }}</span>
              <akd-icon name="chevrons-up-down" [size]="13" />
            }
          </button>
        </div>
      </aside>

      <div class="main">
        <header class="topbar">
          <button
            class="akd-iconbtn"
            type="button"
            (click)="toggleRail()"
            [attr.aria-label]="rail() ? 'Expand sidebar' : 'Collapse sidebar'"
          >
            <akd-icon name="panel-left" [size]="16" />
          </button>
          <nav class="akd-breadcrumb" aria-label="Breadcrumb">
            @if (teamName()) {
              <span class="crumb-team">{{ teamName() }}</span>
              <span class="akd-breadcrumb__sep" aria-hidden="true">/</span>
            }
            <span class="akd-breadcrumb__current">{{ sectionLabel() }}</span>
          </nav>
          <div class="spacer"></div>
          @if (version()) {
            <span class="akd-badge akd-badge--accent akd-badge--mono">v{{ version() }}</span>
          }
          <a class="akd-iconbtn" routerLink="/notifications" aria-label="Notification channels">
            <akd-icon name="bell" [size]="16" />
          </a>
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
        height: 100vh;
        background: var(--surface-page);
      }
      .layout {
        display: grid;
        grid-template-columns: 232px 1fr;
        height: 100vh;
        overflow: hidden;
      }
      .layout.rail {
        grid-template-columns: 60px 1fr;
      }
      .sidebar {
        display: flex;
        flex-direction: column;
        min-height: 0;
        background: var(--bg-1);
        border-right: 1px solid var(--border-1);
        transition: width var(--dur-2) var(--ease-out);
      }
      .brand-logo {
        width: 28px;
        height: 28px;
        border-radius: 6px;
        display: block;
      }
      .brand {
        display: flex;
        align-items: center;
        gap: 10px;
        padding: 16px;
        border-bottom: 1px solid var(--border-1);
        font: var(--weight-bold) 17px var(--font-display);
        color: var(--text-1);
        text-decoration: none;
        white-space: nowrap;
      }
      .rail .brand {
        justify-content: center;
        padding: 16px 0;
      }
      .brand:hover {
        text-decoration: none;
        color: var(--text-1);
      }
      .brand-accent {
        color: var(--accent);
      }
      .nav {
        flex: 1;
        overflow-y: auto;
        padding: 4px 10px;
      }
      .rail .nav {
        padding: 10px 6px;
      }
      .rail .akd-sidenav__item {
        justify-content: center;
        padding: 9px 0;
      }
      .akd-sidenav__item {
        text-decoration: none;
      }
      .akd-sidenav__item:hover {
        text-decoration: none;
      }

      .user {
        position: relative;
      }
      .user-menu {
        position: absolute;
        bottom: 100%;
        left: 10px;
        right: 10px;
        min-width: 220px;
        margin-bottom: 8px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        padding: 6px;
        z-index: 50;
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      .rail .user-menu {
        right: auto;
        width: 240px;
      }
      .user-menu .akd-sidenav__section {
        padding-top: 8px;
      }
      .team-name {
        flex: 1;
        font-family: var(--font-mono);
        font-size: var(--text-sm);
      }
      .current {
        font-size: var(--text-2xs);
        color: var(--text-3);
      }
      .menu-sep {
        height: 1px;
        background: var(--border-1);
        margin: 6px 4px;
      }
      .menu-error {
        padding: 4px 10px 6px;
        font-size: var(--text-2xs);
        color: var(--danger);
      }
      .user-btn {
        all: unset;
        cursor: pointer;
        box-sizing: border-box;
        width: 100%;
        padding: 12px 16px;
        border-top: 1px solid var(--border-1);
        display: flex;
        align-items: center;
        gap: 10px;
        color: var(--text-3);
      }
      .rail .user-btn {
        justify-content: center;
        padding: 12px 0;
      }
      .user-btn.open,
      .user-btn:hover {
        background: var(--bg-2);
      }
      .user-btn:focus-visible {
        box-shadow: var(--ring-focus);
      }
      .avatar {
        width: 26px;
        height: 26px;
        border-radius: 50%;
        background: var(--accent-dim);
        border: 1px solid var(--accent-border);
        display: grid;
        place-content: center;
        font: var(--weight-semibold) 11px var(--font-display);
        color: var(--accent);
        flex: none;
      }
      .email {
        flex: 1;
        font-size: var(--text-sm);
        color: var(--text-2);
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
      }

      .main {
        display: flex;
        flex-direction: column;
        min-width: 0;
        min-height: 0;
      }
      .topbar {
        height: 52px;
        flex: none;
        display: flex;
        align-items: center;
        gap: 14px;
        padding: 0 20px;
        background: var(--bg-1);
        border-bottom: 1px solid var(--border-1);
      }
      .crumb-team {
        font-family: var(--font-mono);
      }
      .spacer {
        flex: 1;
      }
      a.akd-iconbtn {
        text-decoration: none;
      }
      .content {
        flex: 1;
        min-width: 0;
        overflow: auto;
      }
    `,
  ],
})
export class ShellComponent {
  protected readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly sections: NavSection[] = [
    {
      title: 'Platform',
      items: [
        { path: '/projects', label: 'Projects', icon: 'folder-git-2' },
        { path: '/servers', label: 'Servers', icon: 'server' },
        { path: '/sources', label: 'Sources', icon: 'git-branch' },
      ],
    },
    {
      title: 'Operations',
      items: [
        // Bastion targets and the tunnels open onto them (ADR-045). Under
        // Operations rather than Platform: declaring one is rare, watching who
        // is connected to production is not.
        { path: '/external-endpoints', label: 'Tunnels', icon: 'cable' },
        { path: '/notifications', label: 'Notifications', icon: 'bell' },
        { path: '/jobs', label: 'Jobs', icon: 'list-checks' },
        { path: '/events', label: 'Events', icon: 'activity' },
      ],
    },
    {
      title: 'Team',
      items: [
        { path: '/team', label: 'Members', icon: 'users' },
        { path: '/settings', label: 'Settings', icon: 'settings' },
      ],
    },
  ];

  /** Labels for the topbar breadcrumb, keyed by first URL segment. */
  private readonly sectionLabels: Record<string, string> = {
    projects: 'projects',
    applications: 'applications',
    services: 'services',
    databases: 'databases',
    servers: 'servers',
    sources: 'sources',
    'external-endpoints': 'tunnels',
    notifications: 'notifications',
    jobs: 'jobs',
    events: 'events',
    team: 'members',
    settings: 'team settings',
    system: 'global settings',
    security: 'personal settings',
  };

  protected readonly rail = signal(localStorage.getItem('akd.rail') === '1');
  protected readonly menu = signal(false);
  protected readonly teams = signal<MyTeam[]>([]);
  protected readonly version = signal<string | null>(null);
  protected readonly switching = signal(false);
  protected readonly switchError = signal<string | null>(null);

  private readonly url = toSignal(
    this.router.events.pipe(
      filter((e) => e instanceof NavigationEnd),
      map(() => this.router.url),
      startWith(this.router.url),
    ),
    { initialValue: this.router.url },
  );

  protected readonly sectionLabel = computed(() => {
    const first = this.url().split('?')[0].split('/').filter(Boolean)[0] ?? 'projects';
    return this.sectionLabels[first] ?? first;
  });

  /** The team the SESSION acts in, as the server resolved it — never
   *  "the first one listed", which is what made the switcher lie. */
  protected readonly currentTeamUuid = computed(() => this.api.currentUser()?.teamUuid ?? null);

  protected readonly teamName = computed(() => {
    const current = this.currentTeamUuid();
    return this.teams().find((t) => t.uuid === current)?.name ?? null;
  });

  protected readonly initials = computed(() => {
    const name = this.api.currentUser()?.name ?? this.api.currentUser()?.email ?? '';
    const parts = name.split(/[\s.@_-]+/).filter(Boolean);
    return ((parts[0]?.[0] ?? '') + (parts[1]?.[0] ?? '')).toUpperCase() || '?';
  });

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    // Chrome data is decorative: a failure here must not block the page.
    try {
      // The user's MEMBERSHIPS, not the instance's teams: an instance root sees
      // every team through /teams but may only act in the ones they belong to,
      // so listing those here would offer switches the server refuses.
      this.teams.set(await this.api.myTeams());
    } catch {
      /* sidebar simply shows no team */
    }
    try {
      const version = await this.api.client().getVersion();
      this.version.set(version.version ?? null);
    } catch {
      /* no badge */
    }
  }

  protected toggleRail(): void {
    this.rail.update((r) => !r);
    localStorage.setItem('akd.rail', this.rail() ? '1' : '0');
  }

  protected go(path: string): void {
    this.menu.set(false);
    void this.router.navigate([path]);
  }

  /**
   * Switches the session to another team (PRD §37).
   *
   * A full page load, not a router navigation: every open page holds resources
   * of the team we are leaving — a project list, a server, a deployment stream.
   * Reloading is the only way to be sure none of it survives the boundary, and
   * it lands on /projects because the current URL may name a resource that does
   * not exist in the team we are entering.
   */
  protected async switchTeam(uuid: string): Promise<void> {
    if (this.switching() || uuid === this.currentTeamUuid()) {
      this.menu.set(false);
      return;
    }
    this.switching.set(true);
    this.switchError.set(null);
    try {
      await this.api.switchTeam(uuid);
      window.location.assign('/projects');
    } catch (error) {
      this.switchError.set(ApiService.describe(error));
      this.switching.set(false);
    }
  }

  protected async signOut(): Promise<void> {
    this.menu.set(false);
    await this.api.signOut();
    await this.router.navigate(['/sign-in']);
  }
}
