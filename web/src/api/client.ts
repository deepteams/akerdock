// Typed client for the AkerDock API, built on the types generated from the
// OpenAPI contract (ADR-025). `schema.ts` is generated, never edited; this file
// is the thin layer that makes the contract's rules — the ones a type cannot
// express — hard to get wrong.
//
// The rules it encodes:
//   - a write must be idempotent (Idempotency-Key), so a retried request cannot
//     deploy twice;
//   - a sensitive PATCH must carry If-Match, so it cannot silently overwrite a
//     concurrent change (§24.1);
//   - an error body is structured — surface it, never `throw new Error(res)`.

import type { paths, components } from './schema';

export type Error = components['schemas']['Error'];
export type ErrorDetail = components['schemas']['ErrorDetail'];

/** An API call that failed with a structured body — the details matter. */
export class ApiError extends globalThis.Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details: ErrorDetail[] = [],
    readonly requestId?: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }

  /** True when the server refused because state changed under us (§24.1). */
  get isVersionConflict(): boolean {
    return this.status === 409 && this.code === 'version_conflict';
  }

  /** True when a job left objects behind on the server (§20.6.4). */
  get hasRemnants(): boolean {
    return this.status === 409 && this.code === 'remnants_present';
  }
}

export interface ClientOptions {
  baseUrl: string;
  /** Bearer token, for scripts. The dashboard uses the session cookie instead. */
  token?: string;
  /**
   * CSRF token, echoed on every mutation when authenticating by cookie. The
   * server reads it from the akerdock_csrf cookie, which our page can read and
   * a cross-origin attacker cannot.
   */
  csrfToken?: string;
  fetch?: typeof globalThis.fetch;
}

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

interface RequestOptions {
  /** Required by the contract on every sensitive PATCH (§24.1). */
  ifMatch?: number | string;
  /**
   * Makes a write safe to retry: the server replays the original response
   * instead of performing the operation twice. Generated when absent on a
   * POST, because a retry without it is a second deployment.
   */
  idempotencyKey?: string;
  query?: Record<string, string | number | boolean | undefined>;
  body?: unknown;
  signal?: AbortSignal;
}

export class AkerDockClient {
  private readonly baseUrl: string;
  private readonly token?: string;
  private readonly csrfToken?: string;
  private readonly fetchImpl: typeof globalThis.fetch;

  constructor(options: ClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/$/, '');
    this.token = options.token;
    this.csrfToken = options.csrfToken;
    this.fetchImpl = options.fetch ?? globalThis.fetch.bind(globalThis);
  }

  async request<T>(method: Method, path: string, options: RequestOptions = {}): Promise<T> {
    // The dashboard passes a relative baseUrl (/api/v1), which URL cannot
    // parse on its own — anchor it to the page origin. Scripts passing an
    // absolute baseUrl are unaffected: an absolute URL ignores the base, and
    // outside a browser location is undefined.
    const url = new URL(this.baseUrl + path, globalThis.location?.origin);
    for (const [key, value] of Object.entries(options.query ?? {})) {
      if (value !== undefined) url.searchParams.set(key, String(value));
    }

    const headers: Record<string, string> = { Accept: 'application/json' };
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;
    // Echoed on mutations: proof that the request came from our own page and not
    // from a form on someone else's site.
    if (this.csrfToken && method !== 'GET') headers['X-CSRF-Token'] = this.csrfToken;
    if (options.body !== undefined) headers['Content-Type'] = 'application/json';
    if (options.ifMatch !== undefined) headers['If-Match'] = `"${options.ifMatch}"`;
    // A POST without an idempotency key is a POST that deploys twice when the
    // network hiccups. The default is therefore "safe", not "absent".
    if (method === 'POST') {
      headers['Idempotency-Key'] = options.idempotencyKey ?? crypto.randomUUID();
    }

    const response = await this.fetchImpl(url, {
      method,
      headers,
      // The session cookie must ride along; it is HttpOnly, so this is the only
      // way the page can authenticate at all.
      credentials: 'same-origin',
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
    });

    if (response.status === 204) return undefined as T;

    const text = await response.text();
    const payload = text ? (JSON.parse(text) as unknown) : undefined;

    if (!response.ok) {
      const error = (payload ?? {}) as Error;
      throw new ApiError(
        response.status,
        error.code ?? 'unknown',
        error.message ?? response.statusText,
        error.details ?? [],
        error.request_id,
      );
    }
    return payload as T;
  }

  // --- typed shortcuts over the operations the UI actually uses --------------
  // Each return type comes from the contract, so a spec change that removes a
  // field breaks the build here rather than at runtime, in front of a user.

  listApplications(query?: {
    cursor?: string;
    limit?: number;
    project_uuid?: string;
    environment_uuid?: string;
  }) {
    type Response =
      paths['/applications']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/applications', { query });
  }

  getApplication(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}`);
  }

  getApplicationLogs(uuid: string, query?: { lines?: number; component?: string }) {
    type Response =
      paths['/applications/{application_uuid}/logs']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/logs`, { query });
  }

  deployApplication(uuid: string, options: { force?: boolean; idempotencyKey?: string } = {}) {
    type Response =
      paths['/applications/{application_uuid}/deploy']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/deploy`, {
      query: { force: options.force },
      idempotencyKey: options.idempotencyKey,
    });
  }

  /**
   * A sensitive PATCH: `version` is mandatory, and becomes If-Match. Passing it
   * through the type system means a caller cannot forget it and silently
   * clobber someone else's edit (INV-014).
   */
  updateApplication(
    uuid: string,
    version: number,
    body: components['schemas']['ApplicationUpdate'],
  ) {
    type Response =
      paths['/applications/{application_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/applications/${uuid}`, {
      ifMatch: version,
      body,
    });
  }

  listApplicationDeployments(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/applications/{application_uuid}/deployments']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/deployments`, {
      query,
    });
  }

  /** Components of a compose stack (one per compose service, empty otherwise). */
  listApplicationComponents(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/components']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/components`);
  }

  /** PR previews of an application (§20.4). */
  listApplicationPreviews(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/previews']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/previews`);
  }

  /** Maintainer approval of a fork PR preview (INV-010). */
  approvePreviewFork(applicationUuid: string, previewUuid: string) {
    type Response =
      paths['/applications/{application_uuid}/previews/{preview_uuid}/approve']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/applications/${applicationUuid}/previews/${previewUuid}/approve`,
    );
  }

  // --- server proxy (PRD §3) -------------------------------------------------------

  /** start | stop | restart of the server's managed proxy — 202 + job. */
  proxyLifecycle(serverUuid: string, action: 'start' | 'stop' | 'restart') {
    type Response =
      paths['/servers/{server_uuid}/proxy/{action}']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/servers/${serverUuid}/proxy/${action}`);
  }

  getProxyLogs(serverUuid: string, query?: { lines?: number }) {
    type Response =
      paths['/servers/{server_uuid}/proxy/logs']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${serverUuid}/proxy/logs`, { query });
  }

  // --- github apps ---------------------------------------------------------------

  listGithubApps(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/github-apps']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/github-apps', { query });
  }

  createGithubApp(body: components['schemas']['GithubAppCreateRequest']) {
    type Response =
      paths['/github-apps']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/github-apps', { body });
  }

  getGithubApp(uuid: string) {
    type Response =
      paths['/github-apps/{github_app_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/github-apps/${uuid}`);
  }

  deleteGithubApp(uuid: string) {
    return this.request<void>('DELETE', `/github-apps/${uuid}`);
  }

  listGithubAppRepositories(uuid: string, query?: { refresh?: boolean }) {
    type Response =
      paths['/github-apps/{github_app_uuid}/repositories']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/github-apps/${uuid}/repositories`, { query });
  }

  // --- compose stacks (/services) ---------------------------------------------

  listServices(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/services']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/services', { query });
  }

  createService(body: components['schemas']['ServiceCreateRequest']) {
    type Response = paths['/services']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/services', { body });
  }

  getService(uuid: string) {
    type Response =
      paths['/services/{service_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/services/${uuid}`);
  }

  /** Sensitive PATCH: version becomes If-Match (INV-014). */
  updateService(uuid: string, version: number, body: components['schemas']['ServiceUpdate']) {
    type Response =
      paths['/services/{service_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/services/${uuid}`, { ifMatch: version, body });
  }

  deleteService(uuid: string, query?: { delete_volumes?: boolean }) {
    type Response =
      paths['/services/{service_uuid}']['delete']['responses']['202']['content']['application/json'];
    return this.request<Response>('DELETE', `/services/${uuid}`, { query });
  }

  listServiceComponents(uuid: string) {
    type Response =
      paths['/services/{service_uuid}/components']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/services/${uuid}/components`);
  }

  deployService(uuid: string) {
    type Response =
      paths['/services/{service_uuid}/deploy']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/services/${uuid}/deploy`);
  }

  startService(uuid: string) {
    type Response =
      paths['/services/{service_uuid}/start']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/services/${uuid}/start`);
  }

  stopService(uuid: string) {
    type Response =
      paths['/services/{service_uuid}/stop']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/services/${uuid}/stop`);
  }

  restartService(uuid: string) {
    type Response =
      paths['/services/{service_uuid}/restart']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/services/${uuid}/restart`);
  }

  listServiceDeployments(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/services/{service_uuid}/deployments']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/services/${uuid}/deployments`, { query });
  }

  /** Lifecycle (§8). Separate from deploy: starting is not deploying. */
  startApplication(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/start']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/start`);
  }

  stopApplication(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/stop']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/stop`);
  }

  restartApplication(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/restart']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/restart`);
  }

  cancelDeployment(uuid: string) {
    type Response =
      paths['/deployments/{deployment_uuid}/cancel']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/deployments/${uuid}/cancel`);
  }

  getJob(uuid: string) {
    type Response =
      paths['/jobs/{job_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/jobs/${uuid}`);
  }

  listScheduledTasks(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/applications/{application_uuid}/scheduled-tasks']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/scheduled-tasks`, { query });
  }

  runScheduledTask(uuid: string) {
    type Response =
      paths['/scheduled-tasks/{task_uuid}/run']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/scheduled-tasks/${uuid}/run`);
  }

  listTaskExecutions(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/scheduled-tasks/{task_uuid}/executions']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/scheduled-tasks/${uuid}/executions`, { query });
  }

  // --- system -----------------------------------------------------------------

  getHealth() {
    type Response = paths['/health']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/health');
  }

  getVersion() {
    type Response = paths['/version']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/version');
  }

  getInstanceSettings() {
    type Response =
      paths['/system/instance']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/system/instance');
  }

  setInstanceSettings(body: components['schemas']['InstanceIdentityUpdate']) {
    type Response =
      paths['/system/instance']['put']['responses']['200']['content']['application/json'];
    return this.request<Response>('PUT', '/system/instance', { body });
  }

  enableApi() {
    type Response =
      paths['/system/api/enable']['post']['responses']['200']['content']['application/json'];
    return this.request<Response>('POST', '/system/api/enable');
  }

  disableApi() {
    type Response =
      paths['/system/api/disable']['post']['responses']['200']['content']['application/json'];
    return this.request<Response>('POST', '/system/api/disable');
  }

  getTransactionalEmail() {
    type Response =
      paths['/system/email']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/system/email');
  }

  setTransactionalEmail(body: components['schemas']['TransactionalEmailSet']) {
    type Response =
      paths['/system/email']['put']['responses']['200']['content']['application/json'];
    return this.request<Response>('PUT', '/system/email', { body });
  }

  listOauthProviders() {
    type Response =
      paths['/system/oauth-providers']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/system/oauth-providers');
  }

  setOauthProvider(provider: string, body: components['schemas']['OauthProviderSet']) {
    type Response =
      paths['/system/oauth-providers/{oauth_provider}']['put']['responses']['200']['content']['application/json'];
    return this.request<Response>('PUT', `/system/oauth-providers/${provider}`, { body });
  }

  deleteOauthProvider(provider: string) {
    return this.request<void>('DELETE', `/system/oauth-providers/${provider}`);
  }

  getEncryptionStatus() {
    type Response =
      paths['/system/encryption']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/system/encryption');
  }

  rotateEncryption() {
    type Response =
      paths['/system/encryption/rotate']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', '/system/encryption/rotate');
  }

  // --- teams (members, invitations, tokens) ------------------------------------

  listTeams(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/teams']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/teams', { query });
  }

  getTeam(uuid: string) {
    type Response =
      paths['/teams/{team_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/teams/${uuid}`);
  }

  listTeamMembers(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/teams/{team_uuid}/members']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/teams/${uuid}/members`, { query });
  }

  listTeamInvitations(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/teams/{team_uuid}/invitations']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/teams/${uuid}/invitations`, { query });
  }

  createTeamInvitation(uuid: string, body: components['schemas']['InvitationCreate']) {
    type Response =
      paths['/teams/{team_uuid}/invitations']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/teams/${uuid}/invitations`, { body });
  }

  revokeTeamInvitation(teamUuid: string, invitationUuid: string) {
    return this.request<void>('DELETE', `/teams/${teamUuid}/invitations/${invitationUuid}`);
  }

  listApiTokens(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/teams/{team_uuid}/tokens']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/teams/${uuid}/tokens`, { query });
  }

  createApiToken(uuid: string, body: components['schemas']['ApiTokenCreate']) {
    type Response =
      paths['/teams/{team_uuid}/tokens']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/teams/${uuid}/tokens`, { body });
  }

  revokeApiToken(teamUuid: string, tokenUuid: string) {
    return this.request<void>('DELETE', `/teams/${teamUuid}/tokens/${tokenUuid}`);
  }

  // --- projects (+ environments) ------------------------------------------------

  listProjects(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/projects']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/projects', { query });
  }

  createProject(body: components['schemas']['ProjectCreate']) {
    type Response = paths['/projects']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/projects', { body });
  }

  getProject(uuid: string) {
    type Response =
      paths['/projects/{project_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/projects/${uuid}`);
  }

  updateProject(uuid: string, body: components['schemas']['ProjectUpdate']) {
    type Response =
      paths['/projects/{project_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/projects/${uuid}`, { body });
  }

  deleteProject(uuid: string) {
    return this.request<void>('DELETE', `/projects/${uuid}`);
  }

  listEnvironments(projectUuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/projects/{project_uuid}/environments']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/projects/${projectUuid}/environments`, { query });
  }

  createEnvironment(projectUuid: string, body: components['schemas']['EnvironmentCreate']) {
    type Response =
      paths['/projects/{project_uuid}/environments']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/projects/${projectUuid}/environments`, { body });
  }

  getEnvironment(projectUuid: string, environmentUuid: string) {
    type Response =
      paths['/projects/{project_uuid}/environments/{environment_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/projects/${projectUuid}/environments/${environmentUuid}`,
    );
  }

  updateEnvironment(
    projectUuid: string,
    environmentUuid: string,
    body: components['schemas']['EnvironmentUpdate'],
  ) {
    type Response =
      paths['/projects/{project_uuid}/environments/{environment_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'PATCH',
      `/projects/${projectUuid}/environments/${environmentUuid}`,
      { body },
    );
  }

  deleteEnvironment(projectUuid: string, environmentUuid: string) {
    return this.request<void>('DELETE', `/projects/${projectUuid}/environments/${environmentUuid}`);
  }

  // --- private keys --------------------------------------------------------------

  listPrivateKeys(query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/private-keys']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/private-keys', { query });
  }

  createPrivateKey(body: components['schemas']['PrivateKeyCreate']) {
    type Response =
      paths['/private-keys']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/private-keys', { body });
  }

  getPrivateKey(uuid: string, query?: { reveal?: boolean }) {
    type Response =
      paths['/private-keys/{private_key_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/private-keys/${uuid}`, { query });
  }

  updatePrivateKey(uuid: string, version: number, body: components['schemas']['PrivateKeyUpdate']) {
    type Response =
      paths['/private-keys/{private_key_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/private-keys/${uuid}`, { ifMatch: version, body });
  }

  deletePrivateKey(uuid: string) {
    return this.request<void>('DELETE', `/private-keys/${uuid}`);
  }

  // --- webhook endpoints -----------------------------------------------------------

  createWebhookEndpoint(
    applicationUuid: string,
    body: components['schemas']['WebhookEndpointCreate'],
  ) {
    type Response =
      paths['/applications/{application_uuid}/webhook-endpoint']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${applicationUuid}/webhook-endpoint`, {
      body,
    });
  }

  deleteWebhookEndpoint(
    applicationUuid: string,
    provider: paths['/applications/{application_uuid}/webhook-endpoint']['delete']['parameters']['query']['provider'],
  ) {
    return this.request<void>('DELETE', `/applications/${applicationUuid}/webhook-endpoint`, {
      query: { provider },
    });
  }

  // --- terminal sessions (§24.4, ADR-024) --------------------------------------------
  // The response carries a ONE-TIME attach token, shown exactly once: it is
  // redeemed on the /terminal/ws WebSocket, which lives outside the contract
  // (§27.24) — same origin, `?token=…` in the query string.

  createApplicationTerminalSession(applicationUuid: string) {
    type Response =
      paths['/applications/{application_uuid}/terminal-sessions']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${applicationUuid}/terminal-sessions`);
  }

  createDatabaseTerminalSession(databaseUuid: string) {
    type Response =
      paths['/databases/{database_uuid}/terminal-sessions']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/databases/${databaseUuid}/terminal-sessions`);
  }

  createServerTerminalSession(serverUuid: string) {
    type Response =
      paths['/servers/{server_uuid}/terminal-sessions']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/servers/${serverUuid}/terminal-sessions`);
  }

  // --- adoption of existing resources (§20.7, ADR-013/023) ---------------------------

  createAdoptionScan(serverUuid: string) {
    type Response =
      paths['/servers/{server_uuid}/adoption-scans']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/servers/${serverUuid}/adoption-scans`);
  }

  listAdoptionScans(serverUuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/servers/{server_uuid}/adoption-scans']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${serverUuid}/adoption-scans`, { query });
  }

  getAdoptionScan(scanUuid: string) {
    type Response =
      paths['/adoption-scans/{adoption_scan_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/adoption-scans/${scanUuid}`);
  }

  adoptResources(scanUuid: string, body: components['schemas']['AdoptRequest']) {
    type Response =
      paths['/adoption-scans/{adoption_scan_uuid}/adopt']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/adoption-scans/${scanUuid}/adopt`, { body });
  }

  disownApplication(applicationUuid: string) {
    type Response =
      paths['/applications/{application_uuid}/disown']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${applicationUuid}/disown`);
  }

  disownService(serviceUuid: string) {
    type Response =
      paths['/services/{service_uuid}/disown']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/services/${serviceUuid}/disown`);
  }

  // --- notification channels (+ rules) ----------------------------------------------

  listNotificationChannels(query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/notification-channels']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/notification-channels', { query });
  }

  createNotificationChannel(body: components['schemas']['NotificationChannelCreate']) {
    type Response =
      paths['/notification-channels']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/notification-channels', { body });
  }

  getNotificationChannel(uuid: string) {
    type Response =
      paths['/notification-channels/{channel_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/notification-channels/${uuid}`);
  }

  updateNotificationChannel(
    uuid: string,
    version: number,
    body: components['schemas']['NotificationChannelUpdate'],
  ) {
    type Response =
      paths['/notification-channels/{channel_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/notification-channels/${uuid}`, {
      ifMatch: version,
      body,
    });
  }

  deleteNotificationChannel(uuid: string) {
    return this.request<void>('DELETE', `/notification-channels/${uuid}`);
  }

  testNotificationChannel(uuid: string) {
    type Response =
      paths['/notification-channels/{channel_uuid}/test']['post']['responses']['200']['content']['application/json'];
    return this.request<Response>('POST', `/notification-channels/${uuid}/test`);
  }

  listNotificationRules(channelUuid: string) {
    type Response =
      paths['/notification-channels/{channel_uuid}/rules']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/notification-channels/${channelUuid}/rules`);
  }

  createNotificationRule(
    channelUuid: string,
    body: components['schemas']['NotificationRuleCreate'],
  ) {
    type Response =
      paths['/notification-channels/{channel_uuid}/rules']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/notification-channels/${channelUuid}/rules`, { body });
  }

  deleteNotificationRule(channelUuid: string, ruleUuid: string) {
    return this.request<void>('DELETE', `/notification-channels/${channelUuid}/rules/${ruleUuid}`);
  }

  // --- s3 storages -------------------------------------------------------------------

  listS3Storages(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/s3-storages']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/s3-storages', { query });
  }

  createS3Storage(body: components['schemas']['S3StorageCreate']) {
    type Response =
      paths['/s3-storages']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/s3-storages', { body });
  }

  getS3Storage(uuid: string) {
    type Response =
      paths['/s3-storages/{s3_storage_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/s3-storages/${uuid}`);
  }

  updateS3Storage(uuid: string, version: number, body: components['schemas']['S3StorageUpdate']) {
    type Response =
      paths['/s3-storages/{s3_storage_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/s3-storages/${uuid}`, { ifMatch: version, body });
  }

  deleteS3Storage(uuid: string) {
    return this.request<void>('DELETE', `/s3-storages/${uuid}`);
  }

  validateS3Storage(uuid: string) {
    type Response =
      paths['/s3-storages/{s3_storage_uuid}/validate']['post']['responses']['200']['content']['application/json'];
    return this.request<Response>('POST', `/s3-storages/${uuid}/validate`);
  }

  // --- servers (ca, validate, resources, domains, certificates) ------------------------

  listServers(query?: { cursor?: string; limit?: number }) {
    type Response = paths['/servers']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/servers', { query });
  }

  createServer(body: components['schemas']['ServerCreate']) {
    type Response = paths['/servers']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/servers', { body });
  }

  getServer(uuid: string) {
    type Response =
      paths['/servers/{server_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${uuid}`);
  }

  updateServer(uuid: string, version: number, body: components['schemas']['ServerUpdate']) {
    type Response =
      paths['/servers/{server_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/servers/${uuid}`, { ifMatch: version, body });
  }

  deleteServer(uuid: string) {
    return this.request<void>('DELETE', `/servers/${uuid}`);
  }

  getServerCA(uuid: string) {
    type Response =
      paths['/servers/{server_uuid}/ca']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${uuid}/ca`);
  }

  validateServer(uuid: string) {
    type Response =
      paths['/servers/{server_uuid}/validate']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/servers/${uuid}/validate`);
  }

  listServerResources(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/servers/{server_uuid}/resources']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${uuid}/resources`, { query });
  }

  listServerDomains(uuid: string) {
    type Response =
      paths['/servers/{server_uuid}/domains']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${uuid}/domains`);
  }

  listServerCertificates(
    uuid: string,
    query?: {
      cursor?: string;
      limit?: number;
      expiring_within_days?: number;
      status?: components['schemas']['CertificateStatus'];
    },
  ) {
    type Response =
      paths['/servers/{server_uuid}/certificates']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/servers/${uuid}/certificates`, { query });
  }

  // --- certificates ---------------------------------------------------------------------

  getCertificate(uuid: string) {
    type Response =
      paths['/certificates/{certificate_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/certificates/${uuid}`);
  }

  renewCertificate(uuid: string) {
    type Response =
      paths['/certificates/{certificate_uuid}/renew']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/certificates/${uuid}/renew`);
  }

  // --- applications (storages, envs, lifecycle, deploy, rollback) ------------------------

  createApplication(body: components['schemas']['ApplicationCreateRequest']) {
    type Response =
      paths['/applications']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/applications', { body });
  }

  deleteApplication(uuid: string, query?: { delete_volumes?: boolean }) {
    type Response =
      paths['/applications/{application_uuid}']['delete']['responses']['202']['content']['application/json'];
    return this.request<Response>('DELETE', `/applications/${uuid}`, { query });
  }

  listApplicationStorages(uuid: string) {
    type Response =
      paths['/applications/{application_uuid}/storages']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/storages`);
  }

  createApplicationStorage(uuid: string, body: components['schemas']['PersistentStorageCreate']) {
    type Response =
      paths['/applications/{application_uuid}/storages']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/storages`, { body });
  }

  deleteApplicationStorage(applicationUuid: string, storageUuid: string) {
    return this.request<void>('DELETE', `/applications/${applicationUuid}/storages/${storageUuid}`);
  }

  listApplicationEnvs(uuid: string, query?: { cursor?: string; limit?: number; preview?: boolean }) {
    type Response =
      paths['/applications/{application_uuid}/envs']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/applications/${uuid}/envs`, { query });
  }

  createApplicationEnv(
    uuid: string,
    body: components['schemas']['EnvironmentVariableCreate'],
    query?: { preview?: boolean },
  ) {
    type Response =
      paths['/applications/{application_uuid}/envs']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/envs`, { body, query });
  }

  replaceApplicationEnvs(
    uuid: string,
    body: paths['/applications/{application_uuid}/envs']['put']['requestBody']['content']['application/json'],
  ) {
    type Response =
      paths['/applications/{application_uuid}/envs']['put']['responses']['200']['content']['application/json'];
    return this.request<Response>('PUT', `/applications/${uuid}/envs`, { body });
  }

  updateApplicationEnv(
    applicationUuid: string,
    envUuid: string,
    body: components['schemas']['EnvironmentVariableUpdate'],
  ) {
    type Response =
      paths['/applications/{application_uuid}/envs/{env_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/applications/${applicationUuid}/envs/${envUuid}`, {
      body,
    });
  }

  deleteApplicationEnv(applicationUuid: string, envUuid: string) {
    return this.request<void>('DELETE', `/applications/${applicationUuid}/envs/${envUuid}`);
  }

  rollbackApplication(
    uuid: string,
    body?: NonNullable<
      paths['/applications/{application_uuid}/rollback']['post']['requestBody']
    >['content']['application/json'],
  ) {
    type Response =
      paths['/applications/{application_uuid}/rollback']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${uuid}/rollback`, { body });
  }

  // --- deployments (+ webhook deploy) ------------------------------------------------------

  getDeployment(uuid: string) {
    type Response =
      paths['/deployments/{deployment_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/deployments/${uuid}`);
  }

  webhookDeploy(query?: { uuid?: string; tag?: string; force?: boolean }) {
    type Response = paths['/deploy']['get']['responses']['202']['content']['application/json'];
    return this.request<Response>('GET', '/deploy', { query });
  }

  webhookDeployPost(query?: { uuid?: string; tag?: string; force?: boolean }) {
    type Response = paths['/deploy']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', '/deploy', { query });
  }

  // --- dns credentials -----------------------------------------------------------------------

  listDnsCredentials(query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/dns-credentials']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/dns-credentials', { query });
  }

  createDnsCredential(body: components['schemas']['DnsCredentialCreate']) {
    type Response =
      paths['/dns-credentials']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/dns-credentials', { body });
  }

  getDnsCredential(uuid: string) {
    type Response =
      paths['/dns-credentials/{dns_credential_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/dns-credentials/${uuid}`);
  }

  deleteDnsCredential(uuid: string) {
    return this.request<void>('DELETE', `/dns-credentials/${uuid}`);
  }

  // --- registry credentials ---------------------------------------------------------------------

  listRegistryCredentials(query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/registry-credentials']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/registry-credentials', { query });
  }

  createRegistryCredential(body: components['schemas']['RegistryCredentialCreate']) {
    type Response =
      paths['/registry-credentials']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/registry-credentials', { body });
  }

  getRegistryCredential(uuid: string) {
    type Response =
      paths['/registry-credentials/{registry_credential_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/registry-credentials/${uuid}`);
  }

  updateRegistryCredential(
    uuid: string,
    version: number,
    body: components['schemas']['RegistryCredentialUpdate'],
  ) {
    type Response =
      paths['/registry-credentials/{registry_credential_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/registry-credentials/${uuid}`, {
      ifMatch: version,
      body,
    });
  }

  deleteRegistryCredential(uuid: string) {
    return this.request<void>('DELETE', `/registry-credentials/${uuid}`);
  }

  // --- scheduled tasks (run, executions) ------------------------------------------------------

  createScheduledTask(applicationUuid: string, body: components['schemas']['ScheduledTaskCreate']) {
    type Response =
      paths['/applications/{application_uuid}/scheduled-tasks']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/applications/${applicationUuid}/scheduled-tasks`, {
      body,
    });
  }

  getScheduledTask(uuid: string) {
    type Response =
      paths['/scheduled-tasks/{task_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/scheduled-tasks/${uuid}`);
  }

  updateScheduledTask(
    uuid: string,
    version: number,
    body: components['schemas']['ScheduledTaskUpdate'],
  ) {
    type Response =
      paths['/scheduled-tasks/{task_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/scheduled-tasks/${uuid}`, { ifMatch: version, body });
  }

  deleteScheduledTask(uuid: string) {
    return this.request<void>('DELETE', `/scheduled-tasks/${uuid}`);
  }

  // --- databases (lifecycle, backups, executions, restore, drills) -----------------------------

  listDatabases(query?: {
    cursor?: string;
    limit?: number;
    environment_uuid?: string;
    server_uuid?: string;
  }) {
    type Response = paths['/databases']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/databases', { query });
  }

  createPostgresqlDatabase(body: components['schemas']['DatabaseCreatePostgresql']) {
    type Response =
      paths['/databases/postgresql']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/databases/postgresql', { body });
  }

  getDatabase(uuid: string) {
    type Response =
      paths['/databases/{database_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/databases/${uuid}`);
  }

  updateDatabase(uuid: string, version: number, body: components['schemas']['DatabaseUpdate']) {
    type Response =
      paths['/databases/{database_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/databases/${uuid}`, { ifMatch: version, body });
  }

  deleteDatabase(uuid: string, query?: { delete_volumes?: boolean }) {
    type Response =
      paths['/databases/{database_uuid}']['delete']['responses']['202']['content']['application/json'];
    return this.request<Response>('DELETE', `/databases/${uuid}`, { query });
  }

  startDatabase(uuid: string) {
    type Response =
      paths['/databases/{database_uuid}/start']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/databases/${uuid}/start`);
  }

  stopDatabase(uuid: string) {
    type Response =
      paths['/databases/{database_uuid}/stop']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/databases/${uuid}/stop`);
  }

  restartDatabase(uuid: string) {
    type Response =
      paths['/databases/{database_uuid}/restart']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/databases/${uuid}/restart`);
  }

  listBackupPlans(databaseUuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/databases/{database_uuid}/backups']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/databases/${databaseUuid}/backups`, { query });
  }

  createBackupPlan(databaseUuid: string, body: components['schemas']['BackupPlanCreate']) {
    type Response =
      paths['/databases/{database_uuid}/backups']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/databases/${databaseUuid}/backups`, { body });
  }

  getBackupPlan(databaseUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/databases/${databaseUuid}/backups/${backupPlanUuid}`);
  }

  updateBackupPlan(
    databaseUuid: string,
    backupPlanUuid: string,
    version: number,
    body: components['schemas']['BackupPlanUpdate'],
  ) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/databases/${databaseUuid}/backups/${backupPlanUuid}`, {
      ifMatch: version,
      body,
    });
  }

  deleteBackupPlan(databaseUuid: string, backupPlanUuid: string) {
    return this.request<void>('DELETE', `/databases/${databaseUuid}/backups/${backupPlanUuid}`);
  }

  executeBackupPlan(databaseUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}/execute']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/databases/${databaseUuid}/backups/${backupPlanUuid}/execute`,
    );
  }

  runRestoreDrill(databaseUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}/drill']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/databases/${databaseUuid}/backups/${backupPlanUuid}/drill`,
    );
  }

  listRestoreDrills(
    databaseUuid: string,
    backupPlanUuid: string,
    query?: { cursor?: string; limit?: number },
  ) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}/drills']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/databases/${databaseUuid}/backups/${backupPlanUuid}/drills`,
      { query },
    );
  }

  listBackupExecutions(
    databaseUuid: string,
    backupPlanUuid: string,
    query?: { cursor?: string; limit?: number },
  ) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}/executions']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/databases/${databaseUuid}/backups/${backupPlanUuid}/executions`,
      { query },
    );
  }

  restoreBackupExecution(
    databaseUuid: string,
    backupPlanUuid: string,
    executionUuid: string,
    body: components['schemas']['RestoreRequest'],
  ) {
    type Response =
      paths['/databases/{database_uuid}/backups/{backup_plan_uuid}/executions/{execution_uuid}/restore']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/databases/${databaseUuid}/backups/${backupPlanUuid}/executions/${executionUuid}/restore`,
      { body },
    );
  }

  // --- backups of a stack's internal databases (compose-spec §10) --------------------------------

  listComponentBackupPlans(componentUuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/service-components/${componentUuid}/backups`, { query });
  }

  createComponentBackupPlan(
    componentUuid: string,
    body: components['schemas']['BackupPlanCreate'],
  ) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', `/service-components/${componentUuid}/backups`, { body });
  }

  getComponentBackupPlan(componentUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}`,
    );
  }

  updateComponentBackupPlan(
    componentUuid: string,
    backupPlanUuid: string,
    version: number,
    body: components['schemas']['BackupPlanUpdate'],
  ) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'PATCH',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}`,
      { ifMatch: version, body },
    );
  }

  deleteComponentBackupPlan(componentUuid: string, backupPlanUuid: string) {
    return this.request<void>(
      'DELETE',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}`,
    );
  }

  executeComponentBackupPlan(componentUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}/execute']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}/execute`,
    );
  }

  runComponentRestoreDrill(componentUuid: string, backupPlanUuid: string) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}/drill']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}/drill`,
    );
  }

  listComponentRestoreDrills(
    componentUuid: string,
    backupPlanUuid: string,
    query?: { cursor?: string; limit?: number },
  ) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}/drills']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}/drills`,
      { query },
    );
  }

  listComponentBackupExecutions(
    componentUuid: string,
    backupPlanUuid: string,
    query?: { cursor?: string; limit?: number },
  ) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}/executions']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>(
      'GET',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}/executions`,
      { query },
    );
  }

  restoreComponentBackupExecution(
    componentUuid: string,
    backupPlanUuid: string,
    executionUuid: string,
    body: components['schemas']['RestoreRequest'],
  ) {
    type Response =
      paths['/service-components/{service_component_uuid}/backups/{backup_plan_uuid}/executions/{execution_uuid}/restore']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>(
      'POST',
      `/service-components/${componentUuid}/backups/${backupPlanUuid}/executions/${executionUuid}/restore`,
      { body },
    );
  }

  // --- automated docker cleanup (§3.7) ------------------------------------------------------------

  runServerCleanup(serverUuid: string) {
    type Response =
      paths['/servers/{server_uuid}/cleanup']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/servers/${serverUuid}/cleanup`);
  }

  // --- uptime monitoring (ADR-017) ----------------------------------------------------------------

  listUptimeChecks(query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/uptime-checks']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/uptime-checks', { query });
  }

  createUptimeCheck(body: components['schemas']['UptimeCheckCreate']) {
    type Response =
      paths['/uptime-checks']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/uptime-checks', { body });
  }

  getUptimeCheck(uuid: string) {
    type Response =
      paths['/uptime-checks/{uptime_check_uuid}']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/uptime-checks/${uuid}`);
  }

  updateUptimeCheck(
    uuid: string,
    version: number,
    body: components['schemas']['UptimeCheckUpdate'],
  ) {
    type Response =
      paths['/uptime-checks/{uptime_check_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/uptime-checks/${uuid}`, { ifMatch: version, body });
  }

  deleteUptimeCheck(uuid: string) {
    return this.request<void>('DELETE', `/uptime-checks/${uuid}`);
  }

  listUptimeResults(uuid: string, query?: { cursor?: string; limit?: number }) {
    type Response =
      paths['/uptime-checks/{uptime_check_uuid}/results']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', `/uptime-checks/${uuid}/results`, { query });
  }

  // --- shared variables (§5.4) --------------------------------------------------------------------

  listSharedVariables(query?: {
    cursor?: string;
    limit?: number;
    scope?: 'team' | 'project' | 'environment' | 'server';
  }) {
    type Response =
      paths['/shared-variables']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/shared-variables', { query });
  }

  createSharedVariable(body: components['schemas']['SharedVariableCreate']) {
    type Response =
      paths['/shared-variables']['post']['responses']['201']['content']['application/json'];
    return this.request<Response>('POST', '/shared-variables', { body });
  }

  updateSharedVariable(uuid: string, body: components['schemas']['SharedVariableUpdate']) {
    type Response =
      paths['/shared-variables/{shared_variable_uuid}']['patch']['responses']['200']['content']['application/json'];
    return this.request<Response>('PATCH', `/shared-variables/${uuid}`, { body });
  }

  deleteSharedVariable(uuid: string) {
    return this.request<void>('DELETE', `/shared-variables/${uuid}`);
  }

  // --- jobs (retry, forget) ---------------------------------------------------------------------

  listJobs(query?: {
    cursor?: string;
    limit?: number;
    status?: components['schemas']['JobStatus'];
    queue?: string;
    type?: string;
  }) {
    type Response = paths['/jobs']['get']['responses']['200']['content']['application/json'];
    return this.request<Response>('GET', '/jobs', { query });
  }

  retryJob(uuid: string) {
    type Response =
      paths['/jobs/{job_uuid}/retry']['post']['responses']['202']['content']['application/json'];
    return this.request<Response>('POST', `/jobs/${uuid}/retry`);
  }

  forgetJob(uuid: string, body?: components['schemas']['JobForgetRequest']) {
    type Response =
      paths['/jobs/{job_uuid}/forget']['post']['responses']['200']['content']['application/json'];
    return this.request<Response>('POST', `/jobs/${uuid}/forget`, { body });
  }

  /**
   * Build logs of one deployment, as a live stream (§27.24).
   *
   * The stream starts at sequence 0, so it carries the history AND the live
   * tail — there is no separate backfill to stitch in. On a dropped connection
   * the browser reconnects on its own with `Last-Event-ID`, and the server
   * resumes from there; a reconnect that replays a line the caller already has
   * is harmless because the sequence is the line's identity, and the caller
   * keys on it.
   */
  deploymentLogs(uuid: string): EventSource {
    const url = new URL(`${this.baseUrl}/deployments/${uuid}/logs`, globalThis.location.origin);
    if (this.token) url.searchParams.set('access_token', this.token);
    return new EventSource(url, { withCredentials: true });
  }

  /**
   * Server-sent events (ADR-024). Resumption is the point: `lastEventId` picks
   * up exactly where a dropped connection left off, so a reconnect does not
   * replay — or skip — deployment transitions.
   */
  events(options: { lastEventId?: string } = {}): EventSource {
    const url = new URL(`${this.baseUrl}/events`, globalThis.location.origin);
    // EventSource cannot set headers — which is precisely why the session cookie
    // is the right credential here: the browser attaches it on its own, and it
    // never lands in a URL, a log line or a referrer. A bearer token would have
    // had to travel in the query string.
    if (this.token) url.searchParams.set('access_token', this.token);
    if (options.lastEventId) url.searchParams.set('last_event_id', options.lastEventId);
    return new EventSource(url, { withCredentials: true });
  }
}
