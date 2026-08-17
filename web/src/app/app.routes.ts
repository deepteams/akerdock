import { CanActivateChildFn, Routes } from '@angular/router';
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

/**
 * Forced MFA enrollment (§10.2): when the instance requires MFA and the user has
 * no confirmed factor, every screen but Security (which hosts the enrollment
 * flow) is off-limits until they enrol. The server enforces this too — the API
 * refuses a pending session — this guard just keeps the UI coherent.
 */
const mfaEnrolled: CanActivateChildFn = (route) => {
  const api = inject(ApiService);
  const router = inject(Router);
  if (api.currentUser()?.mfaEnrollmentRequired && route.routeConfig?.path !== 'security') {
    return router.createUrlTree(['/security'], { queryParams: { enroll: 'mfa' } });
  }
  return true;
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
    // Invitation acceptance (ADR-038): full-screen and deliberately NOT guarded.
    // An invitee usually has no account yet — that is what the invitation is
    // for — so a guard here would bounce them to a sign-in form for an account
    // nobody has created, which is a dead end on an instance without SSO. The
    // page itself decides: redeem when signed in, otherwise offer to sign up
    // (the server authenticates the link token, not the visitor).
    path: 'invitations/accept',
    loadComponent: () =>
      import('./pages/accept-invitation.component').then((m) => m.AcceptInvitationComponent),
  },
  {
    // Everything else lives inside the shell: one sidebar naming every
    // capability, one guard in front of all of them.
    path: '',
    canActivate: [authenticated],
    canActivateChild: [mfaEnrolled],
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
        path: 'models',
        loadComponent: () => import('./pages/models.component').then((m) => m.ModelsComponent),
      },
      {
        path: 'models/:uuid',
        loadComponent: () =>
          import('./pages/model-detail.component').then((m) => m.ModelDetailComponent),
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

      // External endpoints (bastion, ADR-045). The request-access route is
      // deep-linked by the CLI when a mint comes back access_request_required,
      // so its shape is part of the contract — the server builds this URL.
      {
        path: 'external-endpoints',
        loadComponent: () =>
          import('./pages/external-endpoints.component').then((m) => m.ExternalEndpointsComponent),
      },
      {
        path: 'external-endpoints/:uuid/request-access',
        loadComponent: () =>
          import('./pages/request-endpoint-access.component').then(
            (m) => m.RequestEndpointAccessComponent,
          ),
      },
      // Declared last: the deep-linked route above is the more specific one,
      // and its shape is part of the CLI contract.
      {
        path: 'external-endpoints/:uuid',
        loadComponent: () =>
          import('./pages/external-endpoint-detail.component').then(
            (m) => m.ExternalEndpointDetailComponent,
          ),
      },
      // Ingress endpoints (ADR-060): the mirror of the bastion, relaying a
      // stable public URL to a developer's machine.
      {
        path: 'ingress',
        loadComponent: () => import('./pages/ingress.component').then((m) => m.IngressComponent),
      },

      // Documentation — the manual, filtered to what the session may do. Not
      // guarded by a permission: the page IS the permission filter, and the
      // reader who has nothing but previews still gets the previews page.
      {
        path: 'docs',
        loadComponent: () => import('./pages/docs/docs.component').then((m) => m.DocsComponent),
      },
      {
        path: 'docs/:topic',
        loadComponent: () => import('./pages/docs/docs.component').then((m) => m.DocsComponent),
      },

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
