import type { components } from '../../../api/schema';
import {
  createFormProblem,
  createRequestFromForm,
  emptyCreateForm,
  parseCommaList,
  parseLines,
  settingsFromApplication,
  settingsToUpdate,
} from './application-form';

type Application = components['schemas']['Application'];

function anApplication(overrides: Partial<Application> = {}): Application {
  return {
    uuid: 'app-1',
    name: 'shop',
    project_uuid: 'p-1',
    environment_uuid: 'e-1',
    server_uuid: 's-1',
    source_type: 'docker_image',
    desired_status: 'running',
    observed_status: 'healthy',
    created_at: '2026-07-01T00:00:00Z',
    version: 3,
    ...overrides,
  };
}

describe('parseLines', () => {
  it('splits on newlines, trims, and drops blanks', () => {
    expect(parseLines(' a.example.com \n\n  b.example.com\n')).toEqual([
      'a.example.com',
      'b.example.com',
    ]);
  });

  it('returns [] for an empty textarea', () => {
    expect(parseLines('')).toEqual([]);
  });
});

describe('parseCommaList', () => {
  it('splits on commas, trims, and drops blanks', () => {
    expect(parseCommaList('web, prod ,,')).toEqual(['web', 'prod']);
  });
});

describe('settingsFromApplication / settingsToUpdate round trip', () => {
  it('carries every configuration field of a git application through unchanged', () => {
    const app = anApplication({
      source_type: 'git',
      description: 'the storefront',
      domains: ['shop.example.com', 'shop.example.com/api'],
      ports_exposes: '3000',
      tags: ['web', 'prod'],
      git_repository: 'git@github.com:acme/shop.git',
      git_branch: 'main',
      private_key_uuid: 'pk-1',
      build_pack: 'nixpacks',
      base_directory: '/apps/shop',
      dockerfile_location: '/Dockerfile',
      publish_directory: null,
      watch_paths: ['apps/shop/**'],
      auto_deploy: true,
      use_build_server: true,
      push_registry_credential_uuid: 'rc-9',
      pre_deployment_command: 'bin/backup',
      post_deployment_command: 'bin/warm-cache',
      health_check: {
        enabled: true,
        path: '/healthz',
        port: 3000,
        method: 'GET',
        interval_seconds: 10,
        timeout_seconds: 2,
        retries: 5,
        start_period_seconds: 20,
      },
      limits: {
        memory_limit: '512m',
        memory_reservation: '256m',
        memory_swap: '1g',
        cpu_limit: '0.5',
        cpu_shares: 512,
        cpu_set: '0-1',
      },
    });

    const update = settingsToUpdate(settingsFromApplication(app), 'git');

    expect(update.name).toBe('shop');
    expect(update.description).toBe('the storefront');
    expect(update.domains).toEqual(['shop.example.com', 'shop.example.com/api']);
    expect(update.ports_exposes).toBe('3000');
    expect(update.tags).toEqual(['web', 'prod']);
    expect(update.git_repository).toBe('git@github.com:acme/shop.git');
    expect(update.git_branch).toBe('main');
    expect(update.private_key_uuid).toBe('pk-1');
    expect(update.build_pack).toBe('nixpacks');
    expect(update.base_directory).toBe('/apps/shop');
    expect(update.publish_directory).toBeNull();
    expect(update.watch_paths).toEqual(['apps/shop/**']);
    expect(update.auto_deploy).toBeTrue();
    expect(update.use_build_server).toBeTrue();
    expect(update.push_registry_credential_uuid).toBe('rc-9');
    expect(update.pre_deployment_command).toBe('bin/backup');
    expect(update.post_deployment_command).toBe('bin/warm-cache');
    expect(update.health_check).toEqual(app.health_check);
    expect(update.limits).toEqual(app.limits);
  });

  it('only sends the fields of the application own source type', () => {
    const form = settingsFromApplication(
      anApplication({ docker_image: 'ghcr.io/acme/shop', docker_image_tag: 'v2' }),
    );
    form.gitRepository = 'git@github.com:acme/other.git';
    form.dockerfile = 'FROM scratch';

    const update = settingsToUpdate(form, 'docker_image');

    expect(update.docker_image).toBe('ghcr.io/acme/shop');
    expect(update.docker_image_tag).toBe('v2');
    expect(update.git_repository).toBeUndefined();
    expect(update.dockerfile).toBeUndefined();
    // use_build_server is meaningless for a prebuilt image: nothing is built.
    expect(update.use_build_server).toBeUndefined();
  });

  it('maps empty inputs to null, not to empty strings', () => {
    const form = settingsFromApplication(anApplication({ docker_image: 'nginx' }));
    form.description = '  ';
    form.portsExposes = '';
    form.registryCredentialUuid = '';
    form.preDeploymentCommand = '';

    const update = settingsToUpdate(form, 'docker_image');

    expect(update.description).toBeNull();
    expect(update.ports_exposes).toBeNull();
    expect(update.registry_credential_uuid).toBeNull();
    expect(update.pre_deployment_command).toBeNull();
  });

  it('never patches a blank image or repository over a real one', () => {
    const form = settingsFromApplication(anApplication({ docker_image: 'nginx' }));
    form.dockerImage = '   ';

    expect(settingsToUpdate(form, 'docker_image').docker_image).toBeUndefined();

    const gitForm = settingsFromApplication(anApplication({ source_type: 'git' }));
    gitForm.gitRepository = '';
    gitForm.gitBranch = '';

    const update = settingsToUpdate(gitForm, 'git');
    expect(update.git_repository).toBeUndefined();
    expect(update.git_branch).toBeUndefined();
  });

  it('round-trips the preview trigger fields of a git application', () => {
    const app = anApplication({
      source_type: 'git',
      previews_enabled: true,
      preview_require_label: 'preview',
      preview_comment_commands_enabled: true,
      preview_cancel_obsolete_builds: true,
    });

    const update = settingsToUpdate(settingsFromApplication(app), 'git');

    expect(update.preview_require_label).toBe('preview');
    expect(update.preview_comment_commands_enabled).toBeTrue();
    expect(update.preview_cancel_obsolete_builds).toBeTrue();
  });

  it('round-trips the search-engine setting for every source type', () => {
    const noindexed = settingsToUpdate(settingsFromApplication(anApplication({ noindex: true })), 'git');
    expect(noindexed.noindex).toBeTrue();

    // Absent from the payload = indexable: the default must survive the trip,
    // or an upgrade would silently delist a production site.
    const indexable = settingsToUpdate(settingsFromApplication(anApplication({})), 'docker_image');
    expect(indexable.noindex).toBeFalse();
  });

  it('round-trips a parameterized public webhook route', () => {
    const app = anApplication({
      access_protection: 'sso',
      access_public_routes: [
        {
          path: '/webhook/:provider/handler',
          match: 'template',
          methods: ['POST'],
          parameters: { provider: ['stripe', 'github'] },
        },
      ],
    });

    const update = settingsToUpdate(settingsFromApplication(app), 'docker_image');

    expect(update.access_protection).toBe('sso');
    expect(update.access_public_routes).toEqual([
      {
        path: '/webhook/:provider/handler',
        match: 'template',
        methods: ['POST'],
        parameters: { provider: ['stripe', 'github'] },
      },
    ]);
  });

  it('seeds the preview route table from the legacy template and round-trips it', () => {
    // No table on the app → the single template becomes one editable row.
    const legacy = settingsFromApplication(
      anApplication({ source_type: 'git', preview_url_template: 'varuna-pr{{pr_id}}.ad.kedric.fr' }),
    );
    expect(legacy.previewUrlTemplates).toEqual([
      { host: 'varuna-pr{{pr_id}}.ad.kedric.fr', port: '' },
    ]);

    // Editing the table → typed rows out (port parsed, blank host dropped).
    legacy.previewUrlTemplates = [
      { host: '{{service}}-pr{{pr_id}}.ad.kedric.fr', port: '' },
      { host: 'api-pr{{pr_id}}.ad.kedric.fr', port: '8080' },
      { host: '  ', port: '1' },
    ];
    expect(settingsToUpdate(legacy, 'git').preview_url_templates).toEqual([
      { host: '{{service}}-pr{{pr_id}}.ad.kedric.fr', port: null },
      { host: 'api-pr{{pr_id}}.ad.kedric.fr', port: 8080 },
    ]);
  });

  it('defaults preview_deploy_on_open to true and round-trips it when off', () => {
    // Absent from the API (older instance) reads as the historical behaviour.
    const on = settingsFromApplication(anApplication({ source_type: 'git' }));
    expect(on.previewDeployOnOpen).toBeTrue();

    const off = settingsFromApplication(
      anApplication({ source_type: 'git', preview_deploy_on_open: false }),
    );
    expect(off.previewDeployOnOpen).toBeFalse();
    expect(settingsToUpdate(off, 'git').preview_deploy_on_open).toBeFalse();
  });

  it('sends null for a blank required label — disabled, not the string ""', () => {
    const form = settingsFromApplication(anApplication({ source_type: 'git' }));
    form.previewRequireLabel = '  ';

    expect(settingsToUpdate(form, 'git').preview_require_label).toBeNull();
  });

  it('never sends the git API token unless typed or explicitly cleared', () => {
    const form = settingsFromApplication(
      anApplication({ source_type: 'git', git_api_token_set: true }),
    );

    // Blank field: keep the stored (write-only) token untouched.
    expect(settingsToUpdate(form, 'git').git_api_token).toBeUndefined();

    // Typed value: replace it.
    form.gitApiToken = ' glpat-abc123 ';
    expect(settingsToUpdate(form, 'git').git_api_token).toBe('glpat-abc123');

    // Clear checkbox wins over anything typed: explicit null removes it.
    form.gitApiTokenClear = true;
    expect(settingsToUpdate(form, 'git').git_api_token).toBeNull();
  });

  it('falls back to health check defaults when numeric inputs are garbage', () => {
    const form = settingsFromApplication(anApplication({ docker_image: 'nginx' }));
    form.hcIntervalSeconds = 'abc';
    form.hcTimeoutSeconds = '';
    form.hcPort = 'not-a-port';

    const hc = settingsToUpdate(form, 'docker_image').health_check!;
    expect(hc.interval_seconds).toBe(30);
    expect(hc.timeout_seconds).toBe(5);
    expect(hc.port).toBeNull();
  });
});

describe('createFormProblem', () => {
  function validDockerImageForm() {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.dockerImage = 'nginx';
    return form;
  }

  it('accepts a complete docker_image form', () => {
    expect(createFormProblem(validDockerImageForm())).toBeNull();
  });

  it('requires placement before anything else', () => {
    const form = validDockerImageForm();
    form.environmentUuid = '';
    expect(createFormProblem(form)).toContain('environment');
  });

  it('requires the discriminant fields of the chosen source', () => {
    const form = validDockerImageForm();
    form.sourceType = 'git';
    expect(createFormProblem(form)).toContain('repository');
    form.gitRepository = 'https://github.com/acme/shop';
    expect(createFormProblem(form)).toContain('branch');
    form.gitBranch = 'main';
    expect(createFormProblem(form)).toContain('build pack');
    form.buildPack = 'nixpacks';
    expect(createFormProblem(form)).toBeNull();
  });

  it('refuses a build server without a push registry (amendment #19)', () => {
    const form = validDockerImageForm();
    form.sourceType = 'dockerfile';
    form.dockerfile = 'FROM nginx';
    form.useBuildServer = true;
    expect(createFormProblem(form)).toContain('registry');
    form.pushRegistryCredentialUuid = 'rc-1';
    expect(createFormProblem(form)).toBeNull();
  });
});

describe('settingsFromApplication — preview route table seeding', () => {
  it('maps an existing route table, stringifying a port and blanking a null one', () => {
    const form = settingsFromApplication(
      anApplication({
        source_type: 'git',
        preview_url_templates: [
          { host: 'api-pr{{pr_id}}.ad.kedric.fr', port: 8080 },
          { host: 'pr{{pr_id}}.ad.kedric.fr', port: null },
        ],
      }),
    );
    expect(form.previewUrlTemplates).toEqual([
      { host: 'api-pr{{pr_id}}.ad.kedric.fr', port: '8080' },
      { host: 'pr{{pr_id}}.ad.kedric.fr', port: '' },
    ]);
  });

  it('yields an empty table when the app has neither a route table nor a legacy template', () => {
    // anApplication carries no preview_url_template: the ternary must fall to [].
    expect(settingsFromApplication(anApplication({ source_type: 'git' })).previewUrlTemplates).toEqual(
      [],
    );
  });
});

describe('settingsToUpdate — build-pack specifics', () => {
  it('keeps an explicit docker image tag instead of defaulting to latest', () => {
    const form = settingsFromApplication(
      anApplication({ docker_image: 'nginx', docker_image_tag: 'v2' }),
    );
    expect(settingsToUpdate(form, 'docker_image').docker_image_tag).toBe('v2');
  });

  it('writes the compose file location and raw flag for a compose build pack', () => {
    const form = settingsFromApplication(
      anApplication({
        source_type: 'git',
        build_pack: 'compose',
        compose_file_location: '/stack/docker-compose.yml',
        raw_compose: true,
      }),
    );
    const update = settingsToUpdate(form, 'git');
    expect(update.compose_file_location).toBe('/stack/docker-compose.yml');
    expect(update.raw_compose).toBeTrue();
  });
});

describe('createFormProblem — required-field guards', () => {
  function completeDockerForm() {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.dockerImage = 'nginx';
    return form;
  }

  it('reports a blank name first', () => {
    const form = completeDockerForm();
    form.name = '   ';
    expect(createFormProblem(form)).toContain('Name');
  });

  it('reports a missing project', () => {
    const form = completeDockerForm();
    form.projectUuid = '';
    expect(createFormProblem(form)).toContain('project');
  });

  it('reports a missing server', () => {
    const form = completeDockerForm();
    form.serverUuid = '';
    expect(createFormProblem(form)).toContain('server');
  });

  it('reports a blank docker image', () => {
    const form = completeDockerForm();
    form.dockerImage = '   ';
    expect(createFormProblem(form)).toContain('Docker image');
  });

  it('reports missing dockerfile content', () => {
    const form = completeDockerForm();
    form.sourceType = 'dockerfile';
    form.dockerfile = '  ';
    expect(createFormProblem(form)).toContain('Dockerfile');
  });
});

describe('createRequestFromForm', () => {
  it('builds a docker_image request with the tag defaulted to latest', () => {
    const form = emptyCreateForm();
    form.name = ' shop ';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.dockerImage = ' ghcr.io/acme/shop ';
    form.domains = 'shop.example.com\n';
    form.tags = 'web, prod';
    form.instantDeploy = true;

    const request = createRequestFromForm(form);

    expect(request).toEqual(
      jasmine.objectContaining({
        source_type: 'docker_image',
        name: 'shop',
        docker_image: 'ghcr.io/acme/shop',
        docker_image_tag: 'latest',
        registry_credential_uuid: null,
        domains: ['shop.example.com'],
        tags: ['web', 'prod'],
        instant_deploy: true,
      }),
    );
  });

  it('builds a git request with build defaults filled in', () => {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.sourceType = 'git';
    form.gitRepository = 'https://github.com/acme/shop';
    form.gitBranch = 'main';
    form.buildPack = 'dockerfile';

    const request = createRequestFromForm(form);

    expect(request).toEqual(
      jasmine.objectContaining({
        source_type: 'git',
        git_repository: 'https://github.com/acme/shop',
        git_branch: 'main',
        build_pack: 'dockerfile',
        base_directory: '/',
        dockerfile_location: '/Dockerfile',
        private_key_uuid: null,
        watch_paths: [],
      }),
    );
  });

  it('builds a compose build pack request with its file location defaulted', () => {
    const form = emptyCreateForm();
    form.name = 'stack';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.sourceType = 'git';
    form.gitRepository = 'https://github.com/acme/stack';
    form.gitBranch = 'main';
    form.buildPack = 'compose';

    const request = createRequestFromForm(form);

    expect(request).toEqual(
      jasmine.objectContaining({
        source_type: 'git',
        build_pack: 'compose',
        compose_file_location: '/docker-compose.yml',
        raw_compose: false,
      }),
    );
  });

  it('builds a GitHub App request: discovered repo, no deploy key', () => {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.sourceType = 'git';
    form.githubAppUuid = 'gh-1';
    form.repositoryFullName = 'acme/shop';
    form.gitBranch = 'main';
    form.buildPack = 'nixpacks';
    form.privateKeyUuid = 'pk-should-be-ignored';

    const request = createRequestFromForm(form);

    expect(request).toEqual(
      jasmine.objectContaining({
        source_type: 'git',
        github_app_uuid: 'gh-1',
        repository_full_name: 'acme/shop',
        // A GitHub App clone uses an installation token: a deploy key on top
        // would be two identities for one clone.
        private_key_uuid: null,
      }),
    );
  });

  it('requires a repository when a GitHub App is picked', () => {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.sourceType = 'git';
    form.githubAppUuid = 'gh-1';
    form.gitBranch = 'main';
    form.buildPack = 'nixpacks';
    expect(createFormProblem(form)).toContain('repository');
    form.repositoryFullName = 'acme/shop';
    expect(createFormProblem(form)).toBeNull();
  });

  it('keeps the dockerfile content verbatim — whitespace is syntax there', () => {
    const form = emptyCreateForm();
    form.name = 'shop';
    form.projectUuid = 'p-1';
    form.environmentUuid = 'e-1';
    form.serverUuid = 's-1';
    form.sourceType = 'dockerfile';
    form.dockerfile = 'FROM nginx\nCOPY . /srv\n';

    const request = createRequestFromForm(form);
    expect(request.source_type).toBe('dockerfile');
    expect((request as { dockerfile: string }).dockerfile).toBe('FROM nginx\nCOPY . /srv\n');
  });
});
