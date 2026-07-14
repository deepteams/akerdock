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

      // Resources
      {
        path: 'github-apps',
        loadComponent: () =>
          import('./pages/github-apps.component').then((m) => m.GithubAppsComponent),
      },
      {
        path: 'private-keys',
        loadComponent: () =>
          import('./pages/private-keys.component').then((m) => m.PrivateKeysComponent),
      },
      {
        path: 'registries',
        loadComponent: () =>
          import('./pages/registries.component').then((m) => m.RegistriesComponent),
      },
      {
        path: 'dns-credentials',
        loadComponent: () =>
          import('./pages/dns-credentials.component').then((m) => m.DnsCredentialsComponent),
      },
      {
        path: 's3-storages',
        loadComponent: () =>
          import('./pages/s3-storages.component').then((m) => m.S3StoragesComponent),
      },

      // Instance
      {
        path: 'team',
        loadComponent: () => import('./pages/team.component').then((m) => m.TeamComponent),
      },
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
