import { Routes } from '@angular/router';
import { inject } from '@angular/core';
import { Router } from '@angular/router';
import { ApiService } from './core/api.service';

/**
 * The session lives in an HttpOnly cookie the page cannot read, so "am I logged
 * in?" is a question only the server can answer: the guard asks it (/auth/me)
 * rather than trusting anything stored locally.
 */
const authenticated = async () => {
  const api = inject(ApiService);
  const router = inject(Router);
  if (api.isAuthenticated()) return true;
  if (await api.restore()) return true;
  return router.createUrlTree(['/sign-in']);
};

export const routes: Routes = [
  {
    path: 'sign-in',
    loadComponent: () => import('./pages/sign-in.component').then((m) => m.SignInComponent),
  },
  {
    // CLI login consent (ADR-031): full-screen, guarded so the panel session
    // is established (any login method) before the user approves.
    path: 'cli/authorize',
    canActivate: [authenticated],
    loadComponent: () =>
      import('./pages/cli-authorize.component').then((m) => m.CliAuthorizeComponent),
  },
  {
    // Everything else lives inside the shell: one sidebar naming every
    // capability, one guard in front of all of them.
    path: '',
    canActivate: [authenticated],
    loadComponent: () => import('./layout/shell.component').then((m) => m.ShellComponent),
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'applications' },

      // Deploy
      {
        path: 'projects',
        loadComponent: () => import('./pages/projects.component').then((m) => m.ProjectsComponent),
      },
      {
        path: 'projects/:uuid',
        loadComponent: () =>
          import('./pages/project-detail.component').then((m) => m.ProjectDetailComponent),
      },
      {
        path: 'projects/:uuid/environments/:envUuid',
        loadComponent: () =>
          import('./pages/environment-detail.component').then((m) => m.EnvironmentDetailComponent),
      },
      {
        path: 'applications',
        loadComponent: () =>
          import('./pages/applications.component').then((m) => m.ApplicationsComponent),
      },
      {
        // Static before dynamic: ':uuid' would otherwise swallow 'new'.
        path: 'applications/new',
        loadComponent: () =>
          import('./pages/application-new.component').then((m) => m.ApplicationNewComponent),
      },
      {
        path: 'applications/:uuid',
        loadComponent: () =>
          import('./pages/application-detail.component').then((m) => m.ApplicationDetailComponent),
      },
      {
        path: 'applications/:uuid/previews/:previewUuid',
        loadComponent: () =>
          import('./pages/preview-detail.component').then((m) => m.PreviewDetailComponent),
      },
      {
        path: 'applications/:uuid/deployments/:deploymentUuid',
        loadComponent: () =>
          import('./pages/deployment-detail.component').then((m) => m.DeploymentDetailComponent),
      },
      {
        path: 'services',
        loadComponent: () => import('./pages/services.component').then((m) => m.ServicesComponent),
      },
      {
        path: 'services/:uuid',
        loadComponent: () =>
          import('./pages/service-detail.component').then((m) => m.ServiceDetailComponent),
      },
      {
        path: 'databases',
        loadComponent: () =>
          import('./pages/databases.component').then((m) => m.DatabasesComponent),
      },
      {
        path: 'databases/:uuid',
        loadComponent: () =>
          import('./pages/database-detail.component').then((m) => m.DatabaseDetailComponent),
      },
      {
        path: 'servers',
        loadComponent: () => import('./pages/servers.component').then((m) => m.ServersComponent),
      },
      {
        path: 'servers/:uuid',
        loadComponent: () =>
          import('./pages/server-detail.component').then((m) => m.ServerDetailComponent),
      },

      // Operate
      {
        path: 'jobs',
        loadComponent: () => import('./pages/jobs.component').then((m) => m.JobsComponent),
      },
      {
        path: 'jobs/:uuid',
        loadComponent: () =>
          import('./pages/job-detail.component').then((m) => m.JobDetailComponent),
      },
      {
        path: 'events',
        loadComponent: () => import('./pages/events.component').then((m) => m.EventsComponent),
      },
      {
        path: 'notifications',
        loadComponent: () =>
          import('./pages/notifications.component').then((m) => m.NotificationsComponent),
      },
      {
        path: 'notifications/:uuid',
        loadComponent: () =>
          import('./pages/notification-channel-detail.component').then(
            (m) => m.NotificationChannelDetailComponent,
          ),
      },

      // Sources — one page, five credential families. The old top-level paths
      // redirect to their tab so bookmarks keep working.
      {
        path: 'sources',
        loadComponent: () => import('./pages/sources.component').then((m) => m.SourcesComponent),
      },
      { path: 'github-apps', redirectTo: '/sources?tab=github-apps' },
      { path: 'private-keys', redirectTo: '/sources?tab=private-keys' },
      { path: 'registries', redirectTo: '/sources?tab=registries' },
      { path: 'dns-credentials', redirectTo: '/sources?tab=dns' },
      { path: 's3-storages', redirectTo: '/sources?tab=s3' },

      // Team
      {
        path: 'team',
        loadComponent: () => import('./pages/team.component').then((m) => m.TeamComponent),
      },
      {
        path: 'settings',
        loadComponent: () =>
          import('./pages/team-settings.component').then((m) => m.TeamSettingsComponent),
      },

      // Instance (reached from the user menu)
      {
        path: 'system',
        loadComponent: () => import('./pages/system.component').then((m) => m.SystemComponent),
      },
      {
        path: 'security',
        loadComponent: () => import('./pages/security.component').then((m) => m.SecurityComponent),
      },
    ],
  },
  { path: '**', redirectTo: '' },
];
