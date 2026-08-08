import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { toSignal } from '@angular/core/rxjs-interop';
import { NavigationEnd } from '@angular/router';
import { filter, map, startWith } from 'rxjs';
import { ApiService, InspectableRole, MyTeam } from '../core/api.service';
import { NavigationHistory } from '../core/navigation-history.service';
import { IconComponent } from '../../ui/icon/icon.component';
import { ConfirmHostComponent } from '../../ui/confirm/confirm-host.component';

interface NavItem {
  path: string;
  label: string;
  icon: string;
  /** The read permission the page's list call requires. Absent = always shown. */
  permission?: string;
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
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent, ConfirmHostComponent],
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
          @for (section of visibleSections(); track section.title) {
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
                <div class="akd-sidenav__item current-team" aria-current="true">
                  <akd-icon name="check" [size]="14" />
                  <span class="team-name">{{ teamName() }}</span>
                  <span class="current">current</span>
                </div>

                <!-- One team is not a choice: the sub-menu only exists when
                     there is somewhere else to go. -->
                @if (otherTeams().length > 0) {
                  <button
                    class="akd-sidenav__item"
                    role="menuitem"
                    type="button"
                    [attr.aria-expanded]="teamMenu()"
                    (click)="teamMenu.set(!teamMenu())"
                  >
                    <akd-icon name="users" [size]="14" />
                    <span class="team-name">Switch team</span>
                    <akd-icon [name]="teamMenu() ? 'chevron-down' : 'chevron-right'" [size]="13" />
                  </button>
                  @if (teamMenu()) {
                    @for (team of otherTeams(); track team.uuid) {
                      <button
                        class="akd-sidenav__item team-option"
                        role="menuitem"
                        type="button"
                        [disabled]="switching()"
                        (click)="switchTeam(team.uuid)"
                      >
                        <akd-icon name="users" [size]="14" />
                        <span class="team-name">{{ team.name }}</span>
                      </button>
                    }
                  }
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
              <!-- Role inspection (ADR-058): the roles are offered only to
                   whoever may enter the mode, so the list stays empty (and the
                   entry absent) for everybody else.

                   A hover fly-out rather than the team switcher's accordion:
                   the roles are a short, rarely-used list, and pushing the
                   items below it down every time one hovers the menu would
                   move Sign out under the cursor. -->
              @if (inspectableRoles().length > 0) {
                <div class="menu-sep"></div>
                <div class="flyout">
                  <button
                    class="akd-sidenav__item"
                    type="button"
                    role="menuitem"
                    aria-haspopup="menu"
                    [class.active]="viewAs()"
                  >
                    <akd-icon name="eye" [size]="14" />
                    <span class="team-name">View as</span>
                    @if (viewAs()) {
                      <span class="current">{{ viewAs() }}</span>
                    }
                    <akd-icon name="chevron-right" [size]="13" />
                  </button>

                  <div class="flyout-panel">
                    <div class="flyout-inner" role="menu">
                      @if (viewAs()) {
                        <button
                          class="akd-sidenav__item"
                          role="menuitem"
                          type="button"
                          [disabled]="switching()"
                          (click)="stopViewAs()"
                        >
                          <akd-icon name="check" [size]="14" />
                          <span class="team-name">My own view</span>
                        </button>
                        <div class="menu-sep"></div>
                      }
                      @for (role of inspectableRoles(); track role.name) {
                        <button
                          class="akd-sidenav__item team-option"
                          role="menuitem"
                          type="button"
                          [disabled]="switching() || isViewingAs(role)"
                          (click)="startViewAs(role)"
                        >
                          <akd-icon name="eye" [size]="14" />
                          <span class="team-name">{{ role.name }}</span>
                          @if (isViewingAs(role)) {
                            <span class="current">current</span>
                          }
                        </button>
                      }
                    </div>
                  </div>
                </div>
              }
              <div class="menu-sep"></div>
              <button class="akd-sidenav__item" role="menuitem" (click)="signOut()">
                <akd-icon name="log-out" [size]="14" /><span>Sign out</span>
              </button>
            </div>
          }
          <button
            class="user-btn"
            type="button"
            [class.open]="menu()"
            (click)="toggleMenu()"
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
          <!-- Permanent while the mode is on: a degraded UI with no explanation
               reads as a bug, and an operator who forgot they were inspecting
               would file one. -->
          @if (viewAs()) {
            <span class="viewas-banner">
              <akd-icon name="eye" [size]="14" />
              <span>Viewing as <strong>{{ viewAs() }}</strong></span>
              <button type="button" class="viewas-exit" [disabled]="switching()" (click)="stopViewAs()">
                Exit
              </button>
            </span>
          }
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
    <akd-confirm-host />
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

      /* Hover fly-out (role inspection). Opens on hover AND on keyboard focus:
         a menu reachable only by pointer is a menu half the operators cannot
         use. */
      .flyout {
        position: relative;
      }
      .flyout-panel {
        position: absolute;
        left: 100%;
        bottom: -6px;
        /* The bridge: without this gap belonging to the panel, crossing the
           few pixels between the item and its fly-out closes it. */
        padding-left: 6px;
        display: none;
        z-index: 60;
      }
      .flyout:hover > .flyout-panel,
      .flyout:focus-within > .flyout-panel {
        display: block;
      }
      .flyout-inner {
        min-width: 200px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        padding: 6px;
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      /* The parent entry stays lit while its fly-out is open, so the eye is
         not left wondering which item the panel belongs to. */
      .flyout:hover > .akd-sidenav__item,
      .flyout:focus-within > .akd-sidenav__item,
      .flyout > .akd-sidenav__item.active {
        background: var(--bg-4, var(--bg-2));
        color: var(--text-1);
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
      /* Where we already are: a label, not a destination. */
      .current-team,
      .current-team:hover {
        cursor: default;
        background: none;
        color: var(--text-2);
      }
      .team-option {
        padding-left: 30px;
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
      /* Loud on purpose: this says "what you are looking at is not your own
         view", and it has to survive being ignored for an hour. */
      .viewas-banner {
        display: inline-flex;
        align-items: center;
        gap: 8px;
        padding: 3px 8px;
        border: 1px solid var(--warning, var(--accent));
        border-radius: var(--radius-sm, 6px);
        color: var(--warning, var(--accent));
        font-size: var(--text-2xs);
      }
      .viewas-exit {
        border: 0;
        background: none;
        padding: 0;
        color: inherit;
        font: inherit;
        text-decoration: underline;
        cursor: pointer;
      }
      .viewas-exit:disabled {
        cursor: progress;
        opacity: 0.6;
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
  // Injected here and nowhere else on purpose: the service must start watching
  // navigations from the first one, not from the moment a detail page asks it
  // where the user came from — by then it has missed the answer.
  private readonly history = inject(NavigationHistory);

  private readonly sections: NavSection[] = [
    {
      title: 'Platform',
      items: [
        { path: '/projects', label: 'Projects', icon: 'folder-git-2', permission: 'projects:read' },
        { path: '/servers', label: 'Servers', icon: 'server', permission: 'servers:read' },
        { path: '/sources', label: 'Sources', icon: 'git-branch', permission: 'sources:read' },
      ],
    },
    {
      // Bastion targets and the tunnels open onto them (ADR-045) get their own
      // section rather than sitting under Operations: everything else there
      // answers "what is happening", this one answers "how do I reach it" —
      // the question an operator actually scans the menu for.
      title: 'Remote access',
      items: [
        {
          path: '/external-endpoints',
          label: 'Tunnels',
          icon: 'cable',
          permission: 'external-endpoints:read',
        },
        {
          path: '/ingress',
          label: 'Ingress',
          icon: 'globe',
          permission: 'ingress-endpoints:read',
        },
      ],
    },
    {
      title: 'Operations',
      items: [
        {
          path: '/notifications',
          label: 'Notifications',
          icon: 'bell',
          permission: 'notifications:read',
        },
        { path: '/jobs', label: 'Jobs', icon: 'list-checks', permission: 'deployments:read' },
        { path: '/events', label: 'Events', icon: 'activity', permission: 'audit:read' },
      ],
    },
    {
      title: 'Team',
      items: [
        { path: '/team', label: 'Members', icon: 'users', permission: 'members:read' },
        { path: '/settings', label: 'Settings', icon: 'settings', permission: 'team:read' },
      ],
    },
  ];

  /**
   * The sidebar filtered to what this role may actually open (rbac-matrix):
   * an entry whose page would answer 403 is noise, not navigation. The
   * exhaustive-navigation contract above holds for the roles that hold the
   * permissions; a reviewer simply has a shorter map.
   */
  protected readonly visibleSections = computed(() =>
    this.sections
      .map((section) => ({
        ...section,
        items: section.items.filter((item) => !item.permission || this.api.can(item.permission)),
      }))
      .filter((section) => section.items.length > 0),
  );

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
  /** The "Switch team" sub-menu, collapsed until asked for. */
  protected readonly teamMenu = signal(false);
  /** Roles this session may inspect (ADR-058) — empty for anyone who may not,
   *  which is also what hides the whole section. */
  protected readonly inspectableRoles = signal<InspectableRole[]>([]);
  /** The role currently simulated, straight from the server's answer. */
  protected readonly viewAs = computed(() => this.api.currentUser()?.viewAs ?? null);

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

  /** The teams one can switch TO — the current one is where we already are. */
  protected readonly otherTeams = computed(() =>
    this.teams().filter((t) => t.uuid !== this.currentTeamUuid()),
  );

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
    try {
      // 403 for anyone who may not inspect: the empty list is the answer, and
      // the menu section simply does not appear.
      this.inspectableRoles.set(await this.api.inspectableRoles());
    } catch {
      /* no role inspection for this session */
    }
  }

  /** Closing the user menu collapses the team sub-menu with it: reopening
   *  should show the menu as it was first met, not as it was left. */
  protected toggleMenu(): void {
    this.menu.update((open) => !open);
    if (!this.menu()) {
      this.teamMenu.set(false);
      this.switchError.set(null);
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

  /**
   * Enters or leaves the role-inspection mode (ADR-058).
   *
   * A full page load for the same reason as a team switch: every open page was
   * built from permissions that no longer hold, and half of them are streaming
   * data the simulated role may not read. Landing on /projects avoids a URL the
   * inspected role has no access to — which would open the mode on a 403.
   */
  /** Whether the session is currently inspecting this exact role. The server
   *  answers with the role's own label — a system role's name, or a custom
   *  role's — so the comparison is on that, case-insensitively. */
  protected isViewingAs(role: InspectableRole): boolean {
    const current = this.viewAs();
    return !!current && current.toLowerCase() === (role.role ?? role.name).toLowerCase();
  }

  protected async startViewAs(role: InspectableRole): Promise<void> {
    await this.applyViewAs(role);
  }

  protected async stopViewAs(): Promise<void> {
    await this.applyViewAs(null);
  }

  private async applyViewAs(role: InspectableRole | null): Promise<void> {
    if (this.switching()) return;
    this.switching.set(true);
    this.switchError.set(null);
    try {
      await this.api.setViewAs(role);
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
