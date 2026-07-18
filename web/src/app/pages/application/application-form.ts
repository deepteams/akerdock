import type { components } from '../../../api/schema';

type Application = components['schemas']['Application'];
type ApplicationUpdate = components['schemas']['ApplicationUpdate'];
type ApplicationCreateRequest = components['schemas']['ApplicationCreateRequest'];
type HealthCheckConfig = components['schemas']['HealthCheckConfig'];
type ResourceLimits = components['schemas']['ResourceLimits'];
type SourceType = Application['source_type'];
type BuildPack = 'dockerfile' | 'nixpacks' | 'static' | 'compose';

/**
 * Form state is STRINGS, payloads are TYPES. Inputs hold what the operator
 * typed; these functions are the single place where that text becomes a
 * contract-typed request body — so the translation rules (empty means null,
 * one domain per line, comma-separated tags…) are testable without a DOM.
 */

/** Common editable configuration, shared by the create and settings forms. */
export interface ConfigForm {
  name: string;
  description: string;
  /** One FQDN per line (fqdn, fqdn:port or fqdn/path). */
  domains: string;
  /** Comma-separated internal ports (e.g. "3000"). */
  portsExposes: string;
  /** Comma-separated free-form tags. */
  tags: string;

  // Source: docker_image
  dockerImage: string;
  dockerImageTag: string;
  registryCredentialUuid: string;

  // Source: dockerfile
  dockerfile: string;

  // Source: git
  gitRepository: string;
  gitBranch: string;
  privateKeyUuid: string;
  buildPack: BuildPack | '';
  baseDirectory: string;
  dockerfileLocation: string;
  publishDirectory: string;
  composeFileLocation: string;
  rawCompose: boolean;
  /** One auto-deploy watch pattern per line. */
  watchPaths: string;

  // Build
  useBuildServer: boolean;
  pushRegistryCredentialUuid: string;

  // Deployment hooks
  preDeploymentCommand: string;
  postDeploymentCommand: string;

  // Health check
  hcEnabled: boolean;
  hcPath: string;
  hcPort: string;
  hcMethod: string;
  hcIntervalSeconds: string;
  hcTimeoutSeconds: string;
  hcRetries: string;
  hcStartPeriodSeconds: string;

  // Resource limits
  memoryLimit: string;
  memoryReservation: string;
  memorySwap: string;
  cpuLimit: string;
  cpuShares: string;
  cpuSet: string;
}

/** Settings form: configuration of an EXISTING application (PATCH). */
export interface SettingsForm extends ConfigForm {
  autoDeploy: boolean;
  previewsEnabled: boolean;
  previewUrlTemplate: string;
  previewMaxConcurrent: string;
  previewTtlMinutes: string;
  previewProtection: 'none' | 'basic_auth';
  previewForkApprovalEnabled: boolean;
  previewExcludeDrafts: boolean;
  /** PR label required to get a preview; empty = disabled (every PR). */
  previewRequireLabel: string;
  previewCommentCommandsEnabled: boolean;
  previewCancelObsoleteBuilds: boolean;
  /** New provider API token to store; blank = keep the stored one (write-only). */
  gitApiToken: string;
  /** Explicitly remove the stored token (sends null). */
  gitApiTokenClear: boolean;
}

/** Create form: placement + source discriminant on top of the configuration. */
export interface CreateForm extends ConfigForm {
  projectUuid: string;
  environmentUuid: string;
  serverUuid: string;
  sourceType: SourceType;
  instantDeploy: boolean;
  /** GitHub App source (create-only): discovery replaces the pasted URL. */
  githubAppUuid: string;
  repositoryFullName: string;
}

export function emptyConfigForm(): ConfigForm {
  return {
    name: '',
    description: '',
    domains: '',
    portsExposes: '',
    tags: '',
    dockerImage: '',
    dockerImageTag: '',
    registryCredentialUuid: '',
    dockerfile: '',
    gitRepository: '',
    gitBranch: '',
    privateKeyUuid: '',
    buildPack: '',
    baseDirectory: '',
    dockerfileLocation: '',
    publishDirectory: '',
    composeFileLocation: '',
    rawCompose: false,
    watchPaths: '',
    useBuildServer: false,
    pushRegistryCredentialUuid: '',
    preDeploymentCommand: '',
    postDeploymentCommand: '',
    hcEnabled: false,
    hcPath: '/',
    hcPort: '',
    hcMethod: 'GET',
    hcIntervalSeconds: '30',
    hcTimeoutSeconds: '5',
    hcRetries: '3',
    hcStartPeriodSeconds: '10',
    memoryLimit: '',
    memoryReservation: '',
    memorySwap: '',
    cpuLimit: '',
    cpuShares: '',
    cpuSet: '',
  };
}

export function emptyCreateForm(): CreateForm {
  return {
    ...emptyConfigForm(),
    projectUuid: '',
    environmentUuid: '',
    serverUuid: '',
    sourceType: 'docker_image',
    instantDeploy: false,
    githubAppUuid: '',
    repositoryFullName: '',
  };
}

/** Seeds the settings form from what the API says the application IS. */
export function settingsFromApplication(app: Application): SettingsForm {
  const hc = app.health_check;
  const limits = app.limits;
  return {
    name: app.name,
    description: app.description ?? '',
    domains: (app.domains ?? []).join('\n'),
    portsExposes: app.ports_exposes ?? '',
    tags: (app.tags ?? []).join(', '),
    dockerImage: app.docker_image ?? '',
    dockerImageTag: app.docker_image_tag ?? '',
    registryCredentialUuid: app.registry_credential_uuid ?? '',
    dockerfile: app.dockerfile ?? '',
    gitRepository: app.git_repository ?? '',
    gitBranch: app.git_branch ?? '',
    privateKeyUuid: app.private_key_uuid ?? '',
    buildPack: (app.build_pack as BuildPack | null) ?? '',
    baseDirectory: app.base_directory ?? '',
    dockerfileLocation: app.dockerfile_location ?? '',
    publishDirectory: app.publish_directory ?? '',
    composeFileLocation: app.compose_file_location ?? '',
    rawCompose: app.raw_compose ?? false,
    watchPaths: (app.watch_paths ?? []).join('\n'),
    autoDeploy: app.auto_deploy ?? false,
    previewsEnabled: app.previews_enabled ?? false,
    previewUrlTemplate: app.preview_url_template ?? '{{pr_id}}.{{domain}}',
    previewMaxConcurrent: app.preview_max_concurrent != null ? String(app.preview_max_concurrent) : '',
    previewTtlMinutes: app.preview_ttl_minutes != null ? String(app.preview_ttl_minutes) : '',
    previewProtection: (app.preview_protection as 'none' | 'basic_auth') ?? 'basic_auth',
    previewForkApprovalEnabled: app.preview_fork_approval_enabled ?? false,
    previewExcludeDrafts: app.preview_exclude_drafts ?? false,
    previewRequireLabel: app.preview_require_label ?? '',
    previewCommentCommandsEnabled: app.preview_comment_commands_enabled ?? false,
    previewCancelObsoleteBuilds: app.preview_cancel_obsolete_builds ?? false,
    gitApiToken: '',
    gitApiTokenClear: false,
    useBuildServer: app.use_build_server ?? false,
    pushRegistryCredentialUuid: app.push_registry_credential_uuid ?? '',
    preDeploymentCommand: app.pre_deployment_command ?? '',
    postDeploymentCommand: app.post_deployment_command ?? '',
    hcEnabled: hc?.enabled ?? false,
    hcPath: hc?.path ?? '/',
    hcPort: hc?.port != null ? String(hc.port) : '',
    hcMethod: hc?.method ?? 'GET',
    hcIntervalSeconds: String(hc?.interval_seconds ?? 30),
    hcTimeoutSeconds: String(hc?.timeout_seconds ?? 5),
    hcRetries: String(hc?.retries ?? 3),
    hcStartPeriodSeconds: String(hc?.start_period_seconds ?? 10),
    memoryLimit: limits?.memory_limit ?? '',
    memoryReservation: limits?.memory_reservation ?? '',
    memorySwap: limits?.memory_swap ?? '',
    cpuLimit: limits?.cpu_limit ?? '',
    cpuShares: limits?.cpu_shares != null ? String(limits.cpu_shares) : '',
    cpuSet: limits?.cpu_set ?? '',
  };
}

/** Splits a textarea into trimmed, non-empty lines. */
export function parseLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean);
}

/** Splits a comma-separated input into trimmed, non-empty items. */
export function parseCommaList(text: string): string[] {
  return text
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean);
}

/** Empty input means "not set" — the contract's null, never the string "". */
function orNull(text: string): string | null {
  const trimmed = text.trim();
  return trimmed ? trimmed : null;
}

function intOr(text: string, fallback: number): number {
  const parsed = Number.parseInt(text.trim(), 10);
  return Number.isFinite(parsed) ? parsed : fallback;
}

function intOrNull(text: string): number | null {
  const parsed = Number.parseInt(text.trim(), 10);
  return Number.isFinite(parsed) ? parsed : null;
}

function healthCheckFromForm(form: ConfigForm): HealthCheckConfig {
  return {
    enabled: form.hcEnabled,
    path: form.hcPath.trim() || '/',
    port: intOrNull(form.hcPort),
    method: form.hcMethod.trim() || 'GET',
    interval_seconds: intOr(form.hcIntervalSeconds, 30),
    timeout_seconds: intOr(form.hcTimeoutSeconds, 5),
    retries: intOr(form.hcRetries, 3),
    start_period_seconds: intOr(form.hcStartPeriodSeconds, 10),
  };
}

function limitsFromForm(form: ConfigForm): ResourceLimits {
  return {
    memory_limit: orNull(form.memoryLimit),
    memory_reservation: orNull(form.memoryReservation),
    memory_swap: orNull(form.memorySwap),
    cpu_limit: orNull(form.cpuLimit),
    cpu_shares: intOrNull(form.cpuShares),
    cpu_set: orNull(form.cpuSet),
  };
}

/**
 * Builds the PATCH body. Only the fields of the application's OWN source type
 * are sent: `source_type` is immutable, and sending git fields to a
 * docker_image application would be rejected — or worse, silently stored.
 */
export function settingsToUpdate(form: SettingsForm, sourceType: SourceType): ApplicationUpdate {
  const update: ApplicationUpdate = {
    name: form.name.trim(),
    description: orNull(form.description),
    domains: parseLines(form.domains),
    ports_exposes: orNull(form.portsExposes),
    tags: parseCommaList(form.tags),
    pre_deployment_command: orNull(form.preDeploymentCommand),
    post_deployment_command: orNull(form.postDeploymentCommand),
    health_check: healthCheckFromForm(form),
    limits: limitsFromForm(form),
  };

  switch (sourceType) {
    case 'docker_image': {
      const image = form.dockerImage.trim();
      if (image) update.docker_image = image;
      update.docker_image_tag = form.dockerImageTag.trim() || 'latest';
      update.registry_credential_uuid = orNull(form.registryCredentialUuid);
      break;
    }
    case 'dockerfile': {
      const dockerfile = form.dockerfile.trim();
      if (dockerfile) update.dockerfile = form.dockerfile;
      update.use_build_server = form.useBuildServer;
      update.push_registry_credential_uuid = orNull(form.pushRegistryCredentialUuid);
      break;
    }
    case 'git': {
      const repository = form.gitRepository.trim();
      if (repository) update.git_repository = repository;
      const branch = form.gitBranch.trim();
      if (branch) update.git_branch = branch;
      update.private_key_uuid = orNull(form.privateKeyUuid);
      if (form.buildPack) update.build_pack = form.buildPack;
      update.base_directory = form.baseDirectory.trim() || '/';
      update.dockerfile_location = form.dockerfileLocation.trim() || '/Dockerfile';
      update.publish_directory = orNull(form.publishDirectory);
      if (form.buildPack === 'compose') {
        update.compose_file_location = form.composeFileLocation.trim() || '/docker-compose.yml';
        update.raw_compose = form.rawCompose;
      }
      update.watch_paths = parseLines(form.watchPaths);
      update.auto_deploy = form.autoDeploy;
      update.previews_enabled = form.previewsEnabled;
      update.preview_url_template = form.previewUrlTemplate.trim() || '{{pr_id}}.{{domain}}';
      update.preview_max_concurrent = intOrNull(form.previewMaxConcurrent);
      update.preview_ttl_minutes = intOrNull(form.previewTtlMinutes);
      update.preview_protection = form.previewProtection;
      update.preview_fork_approval_enabled = form.previewForkApprovalEnabled;
      update.preview_exclude_drafts = form.previewExcludeDrafts;
      update.preview_require_label = orNull(form.previewRequireLabel);
      update.preview_comment_commands_enabled = form.previewCommentCommandsEnabled;
      update.preview_cancel_obsolete_builds = form.previewCancelObsoleteBuilds;
      // The token never comes back (write-only): a blank field means "keep the
      // stored one", the clear checkbox means "remove it" (explicit null), and
      // a typed value replaces it.
      if (form.gitApiTokenClear) {
        update.git_api_token = null;
      } else if (form.gitApiToken.trim()) {
        update.git_api_token = form.gitApiToken.trim();
      }
      update.use_build_server = form.useBuildServer;
      update.push_registry_credential_uuid = orNull(form.pushRegistryCredentialUuid);
      break;
    }
  }

  return update;
}

/**
 * What still blocks creation, as a human sentence — or null when the form is
 * submittable. One source of truth for both the disabled state of the button
 * and the message explaining it.
 */
export function createFormProblem(form: CreateForm): string | null {
  if (!form.name.trim()) return 'Name is required.';
  if (!form.projectUuid) return 'Pick a project.';
  if (!form.environmentUuid) return 'Pick an environment.';
  if (!form.serverUuid) return 'Pick a server.';
  switch (form.sourceType) {
    case 'docker_image':
      if (!form.dockerImage.trim()) return 'Docker image is required.';
      break;
    case 'dockerfile':
      if (!form.dockerfile.trim()) return 'Dockerfile content is required.';
      break;
    case 'git':
      if (form.githubAppUuid) {
        if (!form.repositoryFullName) return 'Pick a repository.';
      } else if (!form.gitRepository.trim()) {
        return 'Git repository URL is required.';
      }
      if (!form.gitBranch.trim()) return 'Git branch is required.';
      if (!form.buildPack) return 'Pick a build pack.';
      break;
  }
  // Building elsewhere without a registry to push to leaves the image stranded
  // on the build machine (spec amendment #19) — refuse it up front.
  if (form.sourceType !== 'docker_image' && form.useBuildServer && !form.pushRegistryCredentialUuid)
    return 'A build server needs a push registry credential.';
  return null;
}

/** Builds the POST body, discriminated by the chosen source type. */
export function createRequestFromForm(form: CreateForm): ApplicationCreateRequest {
  const base = {
    name: form.name.trim(),
    description: orNull(form.description),
    project_uuid: form.projectUuid,
    environment_uuid: form.environmentUuid,
    server_uuid: form.serverUuid,
    domains: parseLines(form.domains),
    ports_exposes: orNull(form.portsExposes),
    tags: parseCommaList(form.tags),
    pre_deployment_command: orNull(form.preDeploymentCommand),
    post_deployment_command: orNull(form.postDeploymentCommand),
    health_check: healthCheckFromForm(form),
    limits: limitsFromForm(form),
    instant_deploy: form.instantDeploy,
  };

  switch (form.sourceType) {
    case 'docker_image':
      return {
        ...base,
        source_type: 'docker_image',
        docker_image: form.dockerImage.trim(),
        docker_image_tag: form.dockerImageTag.trim() || 'latest',
        registry_credential_uuid: orNull(form.registryCredentialUuid),
        // A prebuilt image builds nothing: the flag is meaningless here, but
        // the contract marks it required, so send its resting value.
        use_build_server: false,
      };
    case 'dockerfile':
      return {
        ...base,
        source_type: 'dockerfile',
        dockerfile: form.dockerfile,
        use_build_server: form.useBuildServer,
        push_registry_credential_uuid: orNull(form.pushRegistryCredentialUuid),
      };
    case 'git':
      return {
        ...base,
        source_type: 'git',
        git_repository: form.gitRepository.trim(),
        github_app_uuid: orNull(form.githubAppUuid),
        repository_full_name: form.githubAppUuid ? orNull(form.repositoryFullName) : null,
        git_branch: form.gitBranch.trim(),
        private_key_uuid: form.githubAppUuid ? null : orNull(form.privateKeyUuid),
        build_pack: form.buildPack as BuildPack,
        base_directory: form.baseDirectory.trim() || '/',
        dockerfile_location: form.dockerfileLocation.trim() || '/Dockerfile',
        publish_directory: orNull(form.publishDirectory),
        compose_file_location:
          form.buildPack === 'compose'
            ? form.composeFileLocation.trim() || '/docker-compose.yml'
            : '/docker-compose.yml',
        raw_compose: form.buildPack === 'compose' ? form.rawCompose : false,
        watch_paths: parseLines(form.watchPaths),
        use_build_server: form.useBuildServer,
        push_registry_credential_uuid: orNull(form.pushRegistryCredentialUuid),
      };
  }
}
