import type { DocTopic } from './docs.model';

/**
 * The manual, written for the person who uses AkerDock every day rather than
 * for the one who installed it: a team member who ships an application, reads
 * its logs, opens a shell, forwards a port, and occasionally wonders why a
 * button is missing. Instance administration is documented too, at the end,
 * gated on the permissions it needs — a reader who cannot register a server is
 * not shown how to.
 *
 * Every claim here must be true of THIS build. When a capability moves, this
 * file moves with it; the tests next door check the cheap half of that (routes
 * that exist, icons that ship, permissions the catalogue knows).
 */
export const DOC_TOPICS: readonly DocTopic[] = [
  // ── Start here ─────────────────────────────────────────────────────────
  {
    id: 'first-deploy',
    title: 'Deploy your first application',
    icon: 'rocket',
    group: 'Start here',
    summary: 'From a Git repository to a running container with an HTTPS URL.',
    permission: 'applications:create',
    links: [
      { label: 'New application', route: '/applications/new' },
      { label: 'Applications', route: '/applications' },
    ],
    sections: [
      {
        id: 'before',
        title: 'What must already exist',
        blocks: [
          {
            kind: 'p',
            text: 'Applications are deployed onto a server your team already has, into a project and an environment. You do not need to own or configure the server: pick one from the list — the placement step only asks where the container should run.',
          },
          {
            kind: 'ul',
            items: [
              'A **server** registered and validated by an administrator (Servers page, read-only for you).',
              'A **project** and an **environment** — create them yourself from Projects, `production` is created with every project.',
              'For a private repository: a **GitHub App** or a **deploy key** under Sources. You may add these yourself.',
            ],
          },
        ],
      },
      {
        id: 'steps',
        title: 'The five steps',
        blocks: [
          {
            kind: 'steps',
            items: [
              'Open **Applications → New application**, or start from an environment so the placement is pre-filled.',
              'Choose the **placement**: project, environment, server and destination (the Docker network the container joins).',
              'Choose the **source**: a Git repository (public URL, GitHub App or deploy key), an inline Dockerfile, or a pre-built Docker image.',
              'Pick the **build pack** for a Git source — `nixpacks` (auto-detection), `dockerfile`, `static`, or `compose` — and the branch.',
              'Create, then hit **Deploy**. The Deployments tab streams the build log line by line.',
            ],
          },
          {
            kind: 'note',
            text: 'An application with no domain is reachable from other containers on its network under its UUID as hostname — that is enough for a worker or an internal API. Add a domain when it needs to be reached from the outside.',
          },
        ],
      },
      {
        id: 'after',
        title: 'Right after the first deploy',
        blocks: [
          {
            kind: 'ul',
            items: [
              'Add your **domains** under Settings → Routing; the certificate is issued automatically.',
              'Declare the **environment variables** the app reads — a container freezes its environment when it is created, so a variable added later needs *Recreate (apply config)*.',
              'Set a **health check** so rolling updates can switch traffic only once the new container answers.',
              'Turn on **auto-deploy** if you want every push on the branch to ship.',
            ],
          },
        ],
      },
    ],
  },
  {
    id: 'concepts',
    title: 'How the platform is organised',
    icon: 'boxes',
    group: 'Start here',
    summary: 'Team → project → environment → resource, and where a server fits in.',
    sections: [
      {
        id: 'hierarchy',
        title: 'The hierarchy',
        blocks: [
          {
            kind: 'code',
            caption: 'Everything you create hangs somewhere on this tree',
            code: `Team                     isolation boundary — servers, resources, tokens
 └── Project             a product, a client, a repository
      └── Environment    production, staging… + its shared variables
           └── Resource  application | database | compose stack`,
          },
          {
            kind: 'ul',
            items: [
              '**Team** — what your permissions are attached to. You can belong to several and hold a different role in each; the switcher sits at the bottom of the sidebar.',
              '**Project / environment** — grouping and scoping. Variables declared on an environment are visible to every resource in it.',
              '**Server** — the Linux machine the containers actually run on, with its own reverse proxy. Registered by an administrator; you only choose it.',
              '**Destination** — the Docker network on that server your container joins. Resources sharing a network can talk to each other by UUID.',
            ],
          },
          {
            kind: 'note',
            text: 'Each resource has a **UUID** and that UUID is the container name, the network alias and the internal hostname. It is what you connect to from another container, and what you pass to the API and the CLI.',
          },
        ],
      },
      {
        id: 'resources',
        title: 'The three kinds of resource',
        blocks: [
          {
            kind: 'table',
            head: ['Resource', 'What it is', 'Where'],
            rows: [
              [
                'Application',
                'Your code: a Git repo built here, an inline Dockerfile, or an image pulled from a registry',
                'Applications',
              ],
              [
                'Database',
                'A managed PostgreSQL with generated credentials, backup plans and restore',
                'Databases',
              ],
              [
                'Compose stack',
                'A docker-compose file deployed as one unit, with a domain per routed service',
                'Services',
              ],
            ],
          },
        ],
      },
    ],
  },
  {
    id: 'permissions',
    title: 'What your role lets you do',
    icon: 'shield-check',
    group: 'Start here',
    summary: 'Why a button is missing, and who to ask when it is.',
    sections: [
      {
        id: 'roles',
        title: 'The three built-in roles',
        blocks: [
          {
            kind: 'p',
            text: 'A role is a named set of granular permissions. The dashboard hides what your role cannot use — including parts of this manual — so a missing page is an answer, not a bug.',
          },
          {
            kind: 'table',
            head: ['Role', 'In one line'],
            rows: [
              [
                'admin',
                'Everything in the team: members, roles, tokens, servers, keys — but never the instance settings',
              ],
              [
                'member',
                'Everything about the resources: apps, databases, stacks, deploys, secrets, backups, previews, tunnels',
              ],
              ['reviewer', 'Read-only path to the PR previews, and nothing else'],
            ],
          },
          {
            kind: 'p',
            text: 'An administrator may also compose a **custom role** with an arbitrary set of permissions; you then see exactly that set.',
          },
        ],
      },
      {
        id: 'member-limits',
        title: 'What a member deliberately cannot do',
        blocks: [
          {
            kind: 'p',
            text: 'These are the walls you will actually meet. None of them is a bug to report — each is a decision:',
          },
          {
            kind: 'ul',
            items: [
              '**Reveal a secret.** You can write a variable and overwrite it, never read back a masked value (`secrets:reveal` is admin-only). The same holds for a database password (`databases:credentials`) and for a private key.',
              '**Register or configure a server**, restart its proxy, run a root shell on it. You may open a shell *in a container*, not on the host.',
              '**Manage members, roles, invitations and team API tokens.** Your own personal token is yours to create, under Personal settings.',
              '**Declare an external endpoint** for the bastion. You can open a tunnel to one that exists, and request access to it.',
              '**Change instance settings** — email, sign-in providers, encryption, telemetry. That is the instance root.',
            ],
          },
          {
            kind: 'note',
            text: 'An administrator can look at the dashboard through your role — the "View as" entry in the user menu — which is the fastest way to settle "it works for me".',
          },
        ],
      },
      {
        id: 'teams',
        title: 'Working in several teams',
        blocks: [
          {
            kind: 'p',
            text: 'A session acts in exactly one team. Switching team from the user menu reloads the dashboard, and the very next request uses the role you hold in the team you switched into. The choice is remembered for your next sign-in.',
          },
        ],
      },
    ],
  },

  // ── Ship code ──────────────────────────────────────────────────────────
  {
    id: 'applications',
    title: 'Applications',
    icon: 'rocket',
    group: 'Ship code',
    summary: 'Sources, build packs, the settings that matter, and the lifecycle buttons.',
    permission: 'applications:read',
    links: [{ label: 'Applications', route: '/applications' }],
    sections: [
      {
        id: 'tabs',
        title: 'The tabs of an application',
        blocks: [
          {
            kind: 'table',
            head: ['Tab', 'What lives there'],
            rows: [
              ['Overview', 'Status, URLs, current image, the lifecycle actions menu'],
              ['Settings', 'General, Source, Build, Routing, Deployment hooks, Health check, Resource limits, Deploys, PR previews'],
              ['Environment variables', 'Two sets — production and previews — in a table or as a raw `.env`'],
              ['Storages', 'Volumes, bind mounts and file mounts declared for the container'],
              ['Scheduled tasks', 'Crons executed inside the container, with their run history'],
              ['Deployments', 'History, live build logs, cancel, rollback'],
              ['Logs', 'Runtime logs of the container, streamed'],
              ['Previews', 'One instance per open pull request'],
              ['Terminal', 'A shell inside the running container'],
              ['Webhook', 'The endpoint your CI or your Git host calls to deploy'],
              ['Danger', 'Delete, or unadopt (stop managing without destroying)'],
            ],
          },
        ],
      },
      {
        id: 'sources',
        title: 'Where the code comes from',
        blocks: [
          {
            kind: 'table',
            head: ['Source', 'Use it when'],
            rows: [
              ['Git repository', 'The platform clones and builds — public URL, GitHub App, or deploy key for a private repo'],
              ['Dockerfile', 'You want to paste a Dockerfile and have it built as-is'],
              ['Docker image', 'Your CI already builds and pushes; AkerDock only pulls and runs it'],
            ],
          },
          {
            kind: 'p',
            text: 'For a Git source, the build pack decides how the image is produced: `nixpacks` detects the language and writes the Dockerfile for you, `dockerfile` uses yours (with `base directory` and `dockerfile location` for a monorepo), `static` serves a published directory, and `compose` treats a compose file as the definition of a multi-service application.',
          },
        ],
      },
      {
        id: 'lifecycle',
        title: 'The lifecycle actions, and which one you want',
        permission: 'applications:lifecycle',
        blocks: [
          {
            kind: 'table',
            head: ['Action', 'What it does', 'Reach for it when'],
            rows: [
              ['Deploy', 'Clones, builds, starts the new container, switches traffic', 'You shipped code'],
              ['Rebuild (no cache)', 'Same, with every build layer recomputed', 'The build is stale or lying to you'],
              [
                'Recreate (apply config)',
                'Re-creates the container from the image already deployed, with the configuration as it stands — no clone, no build',
                'You edited a variable, a limit or a mount and want it live',
              ],
              ['Restart', 'Restarts the existing container', 'The process is wedged'],
              ['Start / Stop', 'Runs or stops the container without touching its definition', 'Pausing an environment'],
              ['Rollback', 'Goes back to a previously deployed image', 'The last deploy is bad'],
            ],
          },
          {
            kind: 'warn',
            text: '**Restart does not pick up a new environment variable.** A container freezes its environment at creation; restarting hands the process back the values it already had. **Recreate (apply config)** is the one that applies an edited variable without a rebuild.',
          },
          {
            kind: 'code',
            caption: 'The same actions from your shell',
            code: `akerdock app info api                  # status, health, components, last deploy
akerdock app restart api               # …and start | stop
akerdock app deploy run api -f         # deploy, following the build
akerdock app deploy run api --skip-build     # Recreate (apply config)
akerdock app deploy run api --force-rebuild  # Rebuild (no cache)
akerdock app deploy rollback api
akerdock app open api                  # the public URL (--dashboard for this page)`,
          },
        ],
      },
      {
        id: 'settings',
        title: 'Settings worth knowing about',
        permission: 'applications:update',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**Health check** — path, port, interval, retries. Traefik only routes to a healthy container, and a rolling update waits for it; without one, a broken deploy still takes traffic.',
              '**Resource limits** — memory and CPU, actually enforced as cgroups on the container.',
              '**Deployment hooks** — a command before the deploy (in the old container) and after (in the new one). A failing post-command fails the deployment.',
              '**Scale to zero** — stop the container after a period without traffic and wake it on the next request. Trades a cold start for an idle server.',
              '**Search engines** — one switch to answer `X-Robots-Tag: noindex, nofollow` on every domain of the app. Preview URLs are never indexable regardless.',
            ],
          },
        ],
      },
      {
        id: 'danger',
        title: 'Delete versus unadopt',
        permission: 'applications:delete',
        blocks: [
          {
            kind: 'p',
            text: '**Delete** destroys the resource and its containers. **Unadopt** only makes AkerDock forget it: the containers keep running on the server, untouched, and stop being managed. Unadopt is the exit door — nothing here holds your workload hostage.',
          },
        ],
      },
    ],
  },
  {
    id: 'env-vars',
    title: 'Environment variables and secrets',
    icon: 'key',
    group: 'Ship code',
    summary: 'Two sets, build-time versus runtime, shared variables and masking.',
    permission: 'secrets:read',
    sections: [
      {
        id: 'sets',
        title: 'Production and previews are two separate sets',
        blocks: [
          {
            kind: 'p',
            text: 'The Environment variables tab has a switch: **Production** and **Previews**. Nothing is copied implicitly from one to the other — a PR instance never inherits your production credentials, and that is deliberate. Keys a preview needs must exist in the previews set.',
          },
          {
            kind: 'p',
            text: 'Each set can be edited as a table, or in bulk as a raw `.env` in the developer view — paste a whole file and it is parsed key by key.',
          },
        ],
      },
      {
        id: 'build-runtime',
        title: 'Build time versus runtime',
        blocks: [
          {
            kind: 'p',
            text: 'A variable is available to the running process by default. Flag it **available at build time** and it is also passed to the build — which is what a framework baking its public configuration into the bundle needs.',
          },
          {
            kind: 'warn',
            text: 'A value used at build time ends up in the image. Never flag a real secret as build-time unless you accept it living in a layer.',
          },
        ],
      },
      {
        id: 'masking',
        title: 'Masked values',
        blocks: [
          {
            kind: 'p',
            text: 'A variable marked secret is masked once written: the dashboard shows it as redacted and you can overwrite it, never read it back. Revealing a stored secret needs `secrets:reveal`, which members do not have. If you lose a value, rotate it — the platform will not recover it for you.',
          },
        ],
      },
      {
        id: 'shared',
        title: 'Shared variables and interpolation',
        blocks: [
          {
            kind: 'p',
            text: 'A value can reference a variable declared higher up, so a shared endpoint or an API base URL is written once:',
          },
          {
            kind: 'code',
            code: `API_URL={{environment.API_BASE}}/v1
SENTRY_DSN={{team.SENTRY_DSN}}
CORS_ORIGIN={{deployment.url}}`,
          },
          {
            kind: 'ul',
            items: [
              '`{{team.KEY}}`, `{{project.KEY}}`, `{{environment.KEY}}` — declared under **Team settings → Shared variables**, at the scope you choose.',
              '`{{deployment.fqdn}}`, `{{deployment.url}}`, `{{deployment.pr_id}}` — the deployment’s own identity, resolved to the app’s domain in production and to the generated URL in a preview.',
              'Predefined: `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `SOURCE_COMMIT`, `PORT`, `HOST`.',
            ],
          },
          {
            kind: 'note',
            text: 'A preview built from a **fork** never resolves shared references, approved or not: resolving them would hand a stranger’s branch your team’s values.',
          },
        ],
      },
      {
        id: 'apply',
        title: 'Making an edit take effect',
        permission: 'secrets:write',
        blocks: [
          {
            kind: 'p',
            text: 'Saving a variable does not touch the running container. Use **Recreate (apply config)** to apply it without rebuilding, or deploy if you were shipping code anyway.',
          },
          {
            kind: 'code',
            caption: 'From your shell — `--apply` is the recreate, in the same command',
            code: `akerdock app env list api
akerdock app env get DATABASE_URL api
akerdock app env set API_URL=https://api.example.com LOG_LEVEL=debug api --apply
akerdock app env unset LEGACY_FLAG api
akerdock app env list --pr 42 api    # the previews set of PR 42`,
          },
          {
            kind: 'note',
            text: 'A compose stack has the same four verbs under `akerdock svc env`. Masking is the server’s answer, not the client’s: a value you cannot reveal here — `secrets:reveal`, and a token carrying `read:sensitive` — comes back redacted in the terminal too. Without `--apply`, `set` writes and stops there: a variable set and never applied is the mistake the flag exists to prevent.',
          },
        ],
      },
    ],
  },
  {
    id: 'domains',
    title: 'Domains, HTTPS and access protection',
    icon: 'globe',
    group: 'Ship code',
    summary: 'Routing several domains and ports, and walling an app behind SSO.',
    permission: 'applications:update',
    sections: [
      {
        id: 'routing',
        title: 'Declaring routes',
        blocks: [
          {
            kind: 'p',
            text: 'Under Settings → Routing, each row maps one or more domains to a container port. The underlying format is one entry per line:',
          },
          {
            kind: 'code',
            code: `app.example.com              # port inferred
api.example.com:8080         # explicit target port
example.com/checkout         # path-based route, most specific wins`,
          },
          {
            kind: 'ul',
            items: [
              'Point the DNS record at the **server’s** IP: traffic goes straight to it, never through the control plane.',
              'A certificate is issued and renewed automatically for each domain; wildcards go through a DNS-01 credential an administrator registers.',
              'A compose stack routes per service — each routed service gets its own domain.',
            ],
          },
        ],
      },
      {
        id: 'protection',
        title: 'Who can reach the application',
        blocks: [
          {
            kind: 'table',
            head: ['Access protection', 'Effect'],
            rows: [
              ['none', 'Public — the default'],
              ['basic_auth', 'Shared credentials prompted by the proxy'],
              ['sso', 'An AkerDock session with access to this team is required'],
            ],
          },
          {
            kind: 'p',
            text: 'The wall applies to **every** domain of the application, previews included. Paths that must stay open through it — a webhook receiver, a health endpoint, an OAuth callback — are listed as **public routes**, with their methods.',
          },
          {
            kind: 'note',
            text: 'This is the cheap way to keep a staging app off the open internet without touching its code.',
          },
        ],
      },
    ],
  },
  {
    id: 'deployments',
    title: 'Deployments',
    icon: 'hammer',
    group: 'Ship code',
    summary: 'Auto-deploy, build logs, cancelling, rolling back, deploying from CI.',
    permission: 'deployments:read',
    sections: [
      {
        id: 'triggers',
        title: 'What triggers a deployment',
        blocks: [
          {
            kind: 'ul',
            items: [
              'The **Deploy** button, `akerdock app deploy run`, or the API.',
              'A **push** on the configured branch, when auto-deploy is on — through the GitHub App or the application’s own webhook endpoint.',
              'A **pull request** event, when previews are on.',
              'A **scheduled** or external call to the deploy webhook from your CI.',
            ],
          },
          {
            kind: 'p',
            text: 'A commit whose message contains `[skip ci]` or `[skip cd]` is ignored. **Watch paths** narrow auto-deploy to pushes touching certain files — the setting a monorepo cannot live without.',
          },
        ],
      },
      {
        id: 'watching',
        title: 'Following a build',
        blocks: [
          {
            kind: 'p',
            text: 'The Deployments tab lists every run with its status — queued, running, finished, failed — and streams the build log live. Reconnecting resumes the stream where it stopped rather than replaying it from the top.',
          },
          {
            kind: 'ul',
            items: [
              'A deployment records the **configuration changes** it carries, not just the commit — useful when the code is identical and the behaviour is not.',
              'A queued or running deployment can be **cancelled**.',
              'Each server has a concurrency limit and a queue; a burst of pushes queues rather than crushing the box.',
            ],
            permission: 'deployments:cancel',
          },
          {
            kind: 'code',
            caption: 'Trigger, follow and cancel without leaving the terminal',
            code: `akerdock app deploy run api -f     # trigger, and follow the build log
akerdock app deploy run api --skip-build   # apply the config, no rebuild
akerdock app deploy list api       # the history, newest first
akerdock app deploy cancel <deployment-uuid>`,
            permission: 'applications:deploy',
          },
          {
            kind: 'note',
            text: 'A compose stack deploys the same way — `akerdock svc deploy run|list|cancel`. `-f` rides the same stream the Deployments tab reads, so the terminal and the browser show the same lines.',
          },
        ],
      },
      {
        id: 'rollback',
        title: 'Rolling back',
        permission: 'applications:deploy',
        blocks: [
          {
            kind: 'p',
            text: 'Rollback redeploys a previously deployed image by its digest, from the deployment history. It is the fast path back; the durable fix is still a commit.',
          },
          {
            kind: 'code',
            code: `akerdock app deploy rollback api            # the previous deployment
akerdock app deploy rollback api --to <uuid>  # a chosen one`,
          },
          {
            kind: 'note',
            text: 'Rollback is an **application** verb only: no such endpoint exists for a compose stack, which is why `akerdock svc deploy` stops at `run`, `list` and `cancel`.',
          },
        ],
      },
      {
        id: 'ci',
        title: 'Deploying from your own CI',
        permission: 'applications:deploy',
        blocks: [
          {
            kind: 'p',
            text: 'Build and push wherever you like, then ask AkerDock to pull and restart:',
          },
          {
            kind: 'code',
            caption: 'Deploy webhook — a token with the deploy scope is enough',
            code: `curl -X POST "https://<instance>/api/v1/deploy?uuid=<app-uuid>" \\
  -H "Authorization: Bearer $AKERDOCK_TOKEN"

# several resources at once, and a forced rebuild
curl -X POST "https://<instance>/api/v1/deploy?uuid=<a>,<b>&force=true" \\
  -H "Authorization: Bearer $AKERDOCK_TOKEN"`,
          },
          {
            kind: 'p',
            text: 'The **Webhook** tab of the application creates the endpoint your Git host calls, with its secret and signature verification.',
          },
        ],
      },
    ],
  },
  {
    id: 'previews',
    title: 'Pull request previews',
    icon: 'git-branch',
    group: 'Ship code',
    summary: 'One live instance per PR, its own variables, its TTL and fork approval.',
    permission: 'previews:read',
    sections: [
      {
        id: 'enable',
        title: 'Turning them on',
        permission: 'applications:update',
        blocks: [
          {
            kind: 'steps',
            items: [
              'Connect the repository through a **GitHub App** (or configure the application’s webhook for GitLab/Gitea).',
              'Ask an administrator for a **wildcard DNS record** — `*.preview.example.com` pointing at the server.',
              'In Settings → **PR previews**, enable them and set the host template, for example `pr-{{pr_id}}.preview.example.com`.',
              'Fill the **previews** set of environment variables — nothing is inherited from production.',
            ],
          },
          {
            kind: 'ul',
            items: [
              '**Max concurrent** caps how many PR instances may live at once.',
              '**TTL** destroys an idle preview after N minutes; the **Keep** action re-arms it when you still need it.',
            ],
          },
        ],
      },
      {
        id: 'daily',
        title: 'Day to day',
        blocks: [
          {
            kind: 'ul',
            items: [
              'A PR opened redeploys on each new commit; closing or merging it destroys the instance.',
              'The Previews tab lists the open pull requests of the repository — a PR opened before you enabled the feature can be deployed by hand from there.',
              'A preview has its own **Logs**, **Terminal**, **Environment variables** and **Storages** tabs, plus *Redeploy*, *Rebuild (no cache)* and *Recreate (apply config)*.',
              'Its URL is never indexable by search engines.',
            ],
          },
          {
            kind: 'code',
            caption: 'Which PRs are live, and where — without opening the dashboard',
            code: `akerdock app preview list api`,
          },
          {
            kind: 'code',
            caption: 'Driving one — `--pr` carries the PR number everywhere',
            code: `akerdock app logs api --pr 42        # its logs
akerdock app env list --pr 42 api    # its own set of variables
akerdock app preview redeploy --pr 42 api
akerdock app preview keep --pr 42 api    # re-arm the TTL while you debug`,
            permission: 'previews:manage',
          },
          {
            kind: 'note',
            text: 'Approving a fork’s preview is **not** a CLI verb: authorising code you have not written to run is project governance, and it stays here where the context is. `keep` is on the other side of that line — holding a preview alive while you debug on it is debugging.',
            permission: 'previews:manage',
          },
        ],
      },
      {
        id: 'forks',
        title: 'Pull requests from a fork',
        permission: 'previews:manage',
        blocks: [
          {
            kind: 'p',
            text: 'A fork’s PR is not deployed automatically: its branch is code you have not written, and deploying it would run it next to your team’s values. Someone with `previews:manage` **approves** the preview explicitly.',
          },
          {
            kind: 'warn',
            text: 'Even approved, a fork preview never resolves `{{team.*}}`, `{{project.*}}` or `{{environment.*}}` references, and receives no server-level variables. Approving says "run this code", not "hand it our secrets".',
          },
        ],
      },
      {
        id: 'reviewer',
        title: 'Sharing a preview with a reviewer',
        blocks: [
          {
            kind: 'p',
            text: 'The **reviewer** role exists for exactly this: someone invited as a reviewer sees the path down to the previews and their URLs, and nothing else — no logs, no variables, no deploy buttons. The same list answers in a terminal, `akerdock app preview list <app>`, with the same narrow reach.',
          },
        ],
      },
    ],
  },
  {
    id: 'compose',
    title: 'Compose stacks',
    icon: 'boxes',
    group: 'Ship code',
    summary: 'A docker-compose file deployed as one unit, with a domain per service.',
    permission: 'services:read',
    links: [{ label: 'Services', route: '/services' }],
    sections: [
      {
        id: 'what',
        title: 'What a stack is',
        blocks: [
          {
            kind: 'p',
            text: 'A stack is a compose file the platform owns: it deploys every service, isolates them in a network named after the stack UUID, and routes the ones you gave a domain. Services without a domain stay private and reachable by their compose name.',
          },
          {
            kind: 'p',
            text: 'You can paste the compose file inline from **Services → New compose stack**, or build one from a Git repository by choosing the `compose` build pack on an application.',
          },
        ],
      },
      {
        id: 'tabs',
        title: 'Managing one',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**Components** lists the containers of the stack with their status; logs and terminal are per component.',
              '**Compose file** is editable in place, then redeployed.',
              '**Environment variables** are shared by the stack; magic variables such as `SERVICE_FQDN_*`, `SERVICE_URL_*` and `SERVICE_PASSWORD_*` are generated once and kept stable across redeployments.',
              'Deploy, Start, Stop and Restart act on the whole stack.',
              'A stack’s internal database can carry its own **backup plan**, like a managed database.',
            ],
          },
          {
            kind: 'code',
            caption: 'The stack group in the CLI — the verbs a stack actually has',
            code: `akerdock svc list
akerdock svc info shop           # the stack and its components
akerdock svc restart shop        # …and start | stop
akerdock svc deploy run shop -f  # …and deploy list | cancel
akerdock svc env list shop       # …and env get | set | unset, with --apply`,
          },
          {
            kind: 'note',
            text: 'A stack has **no `logs`, `shell` or `port-forward` of its own**, and no `rollback`: those endpoints exist for applications and databases only. Debug a stack’s container through the application that owns it, or from the Components tab above — each card carries the exact command to copy.',
          },
          {
            kind: 'note',
            text: 'Access protection (`basic_auth`, `sso`) applies to every routed service of the stack, not only the first one.',
          },
        ],
      },
    ],
  },
  {
    id: 'storages',
    title: 'Persistent storage',
    icon: 'hard-drive',
    group: 'Ship code',
    summary: 'Volumes, bind mounts and editable file mounts.',
    permission: 'storages:manage',
    sections: [
      {
        id: 'kinds',
        title: 'The three kinds',
        blocks: [
          {
            kind: 'table',
            head: ['Kind', 'What you give', 'Good for'],
            rows: [
              ['Volume', 'A name and a mount path', 'Data that must survive a redeploy — uploads, a database directory'],
              ['Bind mount', 'A host path and a mount path', 'Something already on the server'],
              ['File mount', 'A path and the file content, edited here', 'A config file you want to tweak without rebuilding'],
            ],
          },
          {
            kind: 'p',
            text: 'A volume name is prefixed with the resource UUID, so two applications declaring `data` never collide.',
          },
        ],
      },
      {
        id: 'apply',
        title: 'When it takes effect',
        blocks: [
          {
            kind: 'p',
            text: 'Mounts are part of the container definition: declaring one changes nothing until the container is re-created — **Recreate (apply config)** or a deploy.',
          },
          {
            kind: 'warn',
            text: 'Deleting an application does not restore the data in its volumes. Sharing one volume between containers is discouraged: two writers, one lock, and eventually a corrupt file.',
          },
        ],
      },
    ],
  },
  {
    id: 'scheduled-tasks',
    title: 'Scheduled tasks',
    icon: 'clock',
    group: 'Ship code',
    summary: 'Crons that run a command inside your container, with a history.',
    permission: 'applications:update',
    sections: [
      {
        id: 'create',
        title: 'Declaring one',
        blocks: [
          {
            kind: 'p',
            text: 'A task is a name, a command and a schedule — a cron expression or an alias such as `hourly` or `daily`. It runs inside the resource’s container (a specific component for a stack), so it sees the same environment and the same code as the app.',
          },
          {
            kind: 'code',
            caption: 'Typical entries',
            code: `0 3 * * *     php artisan backup:clean
*/15 * * * *  node scripts/sync.js
daily         rails db:sessions:trim`,
          },
          {
            kind: 'ul',
            items: [
              '**Run now** triggers an execution outside the schedule.',
              'The execution history keeps status and output; failures can raise a notification.',
              'A task does not run while the container is stopped — a scale-to-zero app is not a scheduler.',
            ],
          },
          {
            kind: 'code',
            caption: 'Run one on demand from your shell — the fastest way to find out why it fails',
            code: `akerdock app tasks list api
akerdock app tasks run backup-clean api`,
          },
        ],
      },
    ],
  },

  // ── Run and debug ──────────────────────────────────────────────────────
  {
    id: 'logs',
    title: 'Logs',
    icon: 'scroll-text',
    group: 'Run and debug',
    summary: 'Runtime logs and build logs, in the dashboard or in your terminal.',
    permission: 'logs:read',
    sections: [
      {
        id: 'runtime',
        title: 'Runtime logs',
        blocks: [
          {
            kind: 'p',
            text: 'The Logs tab streams the container’s output, with a component selector for a compose stack and a configurable number of lines. Timestamps follow the target server’s timezone, and HTML in a log line is rendered inert rather than interpreted.',
          },
          {
            kind: 'code',
            caption: 'The same thing from your shell',
            code: `akerdock app logs api -f            # follow
akerdock app logs api -n 500        # last 500 lines
akerdock app logs api --pr 42       # the preview of PR 42
akerdock app logs api --deployment  # the latest build log instead`,
          },
          {
            kind: 'note',
            text: 'Logs are an **application** verb: there is no `akerdock db logs` or `akerdock svc logs`, because no endpoint serves them — a stack’s container is read through the application that owns it, or from the Components tab here.',
          },
        ],
      },
      {
        id: 'build',
        title: 'Build logs',
        permission: 'deployments:read',
        blocks: [
          {
            kind: 'p',
            text: 'A deployment’s log is separate from the container’s: open the run from the Deployments tab. It stays readable after the fact, which is what you want when a nightly deploy failed at 3am.',
          },
        ],
      },
      {
        id: 'events',
        title: 'The Events page',
        permission: 'audit:read',
        blocks: [
          {
            kind: 'p',
            text: 'Events is the live feed of the team: status changes, jobs, deployments as they happen. Useful on a second screen while something is shipping.',
          },
        ],
      },
    ],
  },
  {
    id: 'terminal',
    title: 'Terminal',
    icon: 'terminal',
    group: 'Run and debug',
    summary: 'A shell inside a running container, from the browser or the CLI.',
    permission: 'terminal:open',
    sections: [
      {
        id: 'open',
        title: 'Opening a shell',
        blocks: [
          {
            kind: 'p',
            text: 'The Terminal tab of an application, a preview, a database or a stack component opens a real interactive shell in the container — a full terminal, not a command runner. Sessions reconnect and keep their scrollback.',
          },
          {
            kind: 'code',
            code: `akerdock app shell api             # the application's container
akerdock app shell api -c worker   # a component of a compose stack
akerdock db shell main             # a managed database`,
          },
          {
            kind: 'note',
            text: 'Every session is authenticated, bound to the team you act in, and audited. A shell **on the server itself** is a different permission (`terminal:root`) and belongs to administrators.',
          },
        ],
      },
    ],
  },
  {
    id: 'port-forward',
    title: 'Port-forward and tunnels',
    icon: 'cable',
    group: 'Run and debug',
    summary: 'Reach a container or a private endpoint from your machine, without exposing it.',
    permission: 'port-forwards:open',
    sections: [
      {
        id: 'basics',
        title: 'Forwarding a port',
        blocks: [
          {
            kind: 'p',
            text: 'A port-forward opens a TCP tunnel from `127.0.0.1` on your machine to a port of a container — the database keeps no public port, and your local client talks to it as if it were local.',
          },
          {
            kind: 'code',
            code: `akerdock db port-forward 15432:5432 main
psql postgres://user@127.0.0.1:15432/app

akerdock app port-forward 8080:3000 api --pr 42   # into a PR preview
akerdock db console main                          # forward + launch psql`,
          },
          {
            kind: 'ul',
            items: [
              'The **ports come first and the name last** — one look tells the two positionals apart, and the group already said which kind of resource you are aiming at.',
              'The tunnel rides HTTP/3, falls back to HTTP/2, then to WebSocket — it only ever needs 80/443, so a corporate proxy does not break it.',
              'Open sessions are listed in the dashboard, and from your shell with `akerdock tunnel list`; either surface can close one.',
              'Sessions are audited: who opened what, towards which target, and for how long.',
            ],
          },
        ],
      },
      {
        id: 'endpoints',
        title: 'External endpoints (the bastion)',
        permission: 'external-endpoints:read',
        blocks: [
          {
            kind: 'p',
            text: 'An **external endpoint** is a host:port outside the platform — a managed database, a partner’s API — that an administrator declares once. You then reach it through the same tunnel, without holding its network credentials or a VPN:',
          },
          {
            kind: 'code',
            code: `akerdock tunnel open prod-replica         # on a port the OS picks
akerdock tunnel open prod-replica 15432   # on a port you choose

akerdock tunnel list                      # every tunnel open in the team
akerdock tunnel close <session-uuid>`,
          },
          {
            kind: 'p',
            text: 'The remote port is not yours to choose: the endpoint froze its host and port when it was declared — which is why this is `akerdock tunnel`, its own top-level command, and not a `port-forward`, whose targets are the containers the platform deploys and which therefore lives under `app` and `db`. If the endpoint requires an approval, the CLI answers `access_request_required` and hands you the link to **request access**; a granted access has an expiry and can be revoked.',
          },
        ],
      },
      {
        id: 'ingress',
        title: 'Ingress — a public URL onto your laptop',
        permission: 'ingress-tunnels:open',
        blocks: [
          {
            kind: 'p',
            text: 'Ingress is the mirror image: a stable public URL that relays to a port on **your** machine, for a webhook you are debugging or a demo of a branch that never left your editor.',
          },
          {
            kind: 'code',
            code: `akerdock ingress dev-kedric 3000
# https://dev-kedric.<ingress domain> → localhost:3000`,
          },
          {
            kind: 'ul',
            items: [
              'The endpoint’s FQDN is stable — declared once, so a webhook registered upstream keeps working across sessions.',
              'It is walled behind SSO by default; opening it to the public is an explicit choice.',
              'Live tunnels are visible in the dashboard, with their audit trail.',
            ],
          },
        ],
      },
    ],
  },
  {
    id: 'databases',
    title: 'Databases',
    icon: 'database',
    group: 'Run and debug',
    summary: 'Managed PostgreSQL: connecting, lifecycle and what stays admin-only.',
    permission: 'databases:read',
    links: [{ label: 'Databases', route: '/databases' }],
    sections: [
      {
        id: 'create',
        title: 'Creating one',
        permission: 'databases:create',
        blocks: [
          {
            kind: 'p',
            text: 'From Databases, create a **PostgreSQL** instance in a project and environment — the managed engine of this version. Credentials are generated for you; image, tag, resource limits, health check and custom configuration are editable.',
          },
        ],
      },
      {
        id: 'connect',
        title: 'Connecting to it',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**From an application on the same network** — use the database UUID as hostname; nothing needs to be exposed.',
              '**From your machine** — `akerdock db console <name>` opens a tunnel and launches `psql`; `akerdock db port-forward 15432:5432 <name>` leaves the tunnel open for your own client. For a database service inside a compose stack, name the application instead: `akerdock db console --app <app> -c <service>`.',
              '**From the dashboard** — the Database shell card opens `psql` in the container; `akerdock db shell <name>` is the same shell from your terminal.',
            ],
          },
          {
            kind: 'note',
            text: 'The full connection string with its password needs `databases:credentials`, which members do not hold. The Connection card still shows you everything else, and the tunnel works without ever showing you the password.',
          },
        ],
      },
      {
        id: 'lifecycle',
        title: 'Lifecycle',
        permission: 'databases:lifecycle',
        blocks: [
          {
            kind: 'p',
            text: 'Start, stop and restart from the detail page, or from your shell — `akerdock db restart <name>`, and `start` / `stop` alongside it. Stopping a database stops everything that depends on it — check the environment before you do it on a shared instance. There is no confirmation prompt in the terminal: the platform can start it again.',
          },
        ],
      },
    ],
  },
  {
    id: 'backups',
    title: 'Backups and restore',
    icon: 'archive',
    group: 'Run and debug',
    summary: 'Backup plans, retention, restoring, and the drill that proves it works.',
    permission: 'backups:read',
    sections: [
      {
        id: 'plans',
        title: 'Backup plans',
        permission: 'backups:manage',
        blocks: [
          {
            kind: 'p',
            text: 'A plan is a schedule, a destination and a retention policy, attached to a database (or to the internal database of a compose stack).',
          },
          {
            kind: 'ul',
            items: [
              '**Schedule** — a cron expression or an alias (`hourly`, `daily`, `weekly`…), plus a **Backup now** button.',
              '**Destination** — local on the server, an S3 storage, or both; "S3 only" deletes the local file after upload.',
              '**Retention** — max count, max age and max total size, applied separately to local and S3.',
              'Each run is recorded with its status, size and upload result, and can be downloaded or deleted.',
            ],
          },
          {
            kind: 'code',
            caption: 'The same two questions from your shell',
            code: `akerdock db backups list main          # the plans and their executions
akerdock db backups run main           # back up now
akerdock db backups run main --plan nightly`,
          },
          {
            kind: 'note',
            text: 'The CLI stops there: **no `restore`** — overwriting a production database is not an act for a one-line terminal confirmation, so it keeps the dashboard’s context — and no download, because no endpoint serves the file.',
          },
        ],
      },
      {
        id: 'restore',
        title: 'Restoring',
        permission: 'backups:restore',
        blocks: [
          {
            kind: 'p',
            text: 'Restore a database from any recorded execution, straight from its backup list. It is a destructive operation on the target — the dashboard asks you to confirm what you are overwriting, and it is the only surface that offers it: the CLI has no `restore`, by decision.',
          },
        ],
      },
      {
        id: 'drills',
        title: 'Restore drills',
        blocks: [
          {
            kind: 'p',
            text: 'A drill restores a backup into a throwaway instance and reports whether it worked, without touching production. The history is kept next to the plan. A backup nobody has ever restored is not a backup — the drill is what turns the assumption into a fact.',
          },
        ],
      },
    ],
  },
  {
    id: 'notifications',
    title: 'Notifications',
    icon: 'bell',
    group: 'Run and debug',
    summary: 'Channels, routing rules, and the events you can subscribe to.',
    permission: 'notifications:read',
    links: [{ label: 'Notifications', route: '/notifications' }],
    sections: [
      {
        id: 'channels',
        title: 'Channels',
        permission: 'notifications:manage',
        blocks: [
          {
            kind: 'p',
            text: 'A channel is a destination — email, Slack, Discord, Telegram, or a custom webhook — with a **Send a test message** button so you find out it is misconfigured now rather than during an incident.',
          },
        ],
      },
      {
        id: 'events',
        title: 'What you can be told about',
        blocks: [
          {
            kind: 'ul',
            items: [
              'Deployments — succeeded, failed, cancelled.',
              'Previews — created, updated, expiring soon, deleted.',
              'Backups — failed or partial, and restore drills that failed.',
              'Servers — unreachable, recovered, disk cleanup results, certificates expiring.',
              'Scheduled tasks — succeeded or failed; uptime checks — failed or recovered.',
              'Jobs that ended up dead-lettered.',
            ],
          },
          {
            kind: 'p',
            text: 'Each event is toggled per channel, and **routing rules** narrow a channel further — so the on-call channel gets failures and the team channel gets the rest, instead of everyone muting everything.',
          },
        ],
      },
    ],
  },
  {
    id: 'jobs',
    title: 'Jobs',
    icon: 'list-checks',
    group: 'Run and debug',
    summary: 'Where an asynchronous operation went, and why it stopped.',
    permission: 'deployments:read',
    links: [{ label: 'Jobs', route: '/jobs' }],
    sections: [
      {
        id: 'reading',
        title: 'Reading a job',
        blocks: [
          {
            kind: 'p',
            text: 'Deployments, backups, cleanups and cross-server operations run as jobs. The Jobs page lists them with their state; a job’s page shows its steps, its result or its error, and any remnants left on the server when it failed mid-way.',
          },
          {
            kind: 'p',
            text: 'A job that failed every retry is **dead-lettered** — kept rather than dropped, so nothing silently vanishes. Retrying or forgetting one is an administrator action (`jobs:manage`).',
          },
        ],
      },
    ],
  },

  // ── Automate ───────────────────────────────────────────────────────────
  {
    id: 'cli',
    title: 'The CLI',
    icon: 'terminal',
    group: 'Automate',
    summary: 'Log in, target a repository, and drive the platform from your shell.',
    sections: [
      {
        id: 'login',
        title: 'Signing in',
        blocks: [
          {
            kind: 'code',
            code: `akerdock login --url https://akerdock.example.com
akerdock context list          # several instances
akerdock context use staging
akerdock whoami                # where you point, and as whom`,
          },
          {
            kind: 'p',
            text: 'Login opens your browser, works with SSO, and needs no open port on your machine. The credential lands in `~/.akerdock/credentials.yaml` (mode `0600`), separate from the configuration so the latter stays shareable.',
          },
          {
            kind: 'p',
            text: '`whoami` answers from that file alone — context, instance, team and the scopes your token carries, with no network call and never the token itself. It is the question worth asking before a command that stops something.',
          },
        ],
      },
      {
        id: 'shape',
        title: 'How a command is spelled',
        blocks: [
          {
            kind: 'p',
            text: 'The tree is typed: **`akerdock <type> <verb> [NAME]`**. Every verb that acts on one kind of resource lives under that kind’s group, and the name comes last.',
          },
          {
            kind: 'code',
            caption: 'What each group offers — `--help` on any of them is the authoritative list',
            code: `akerdock app   list · info · logs · shell · port-forward · open
               restart · start · stop
               deploy run|list|cancel|rollback
               env list|get|set|unset
               preview list|redeploy|keep
               tasks list|run
akerdock db    list · info · shell · console · port-forward
               restart · start · stop
               backups list|run
akerdock svc   list · info · restart · start · stop
               deploy run|list|cancel
               env list|get|set|unset

akerdock login · logout · context · whoami · list
               tunnel · ingress · mcp · version`,
          },
          {
            kind: 'ul',
            items: [
              'A group offers **the verbs its type actually has**, and not one more — a database has no `deploy`, an application has no `console`, a stack has neither `logs` nor `shell`. The asymmetry is the API’s, and stating it in `--help` beats discovering it at runtime.',
              '`list` is the spelling everywhere — `app list`, `db backups list`, `tunnel list`; `ls` stays an accepted alias.',
              '`-a`, `-e`, `-p` are the short forms of `--application`, `--environment`, `--project`. A positional name always wins over `-a`, which carries the default rather than the target.',
              'Output is a table; `-o json` returns the API objects unaltered, for scripting. Exit codes: `0` success, `1` error, `2` usage — a typo and a failed deployment are told apart.',
            ],
          },
          {
            kind: 'note',
            text: 'The old `type/name` form is gone. `akerdock logs app/api` is refused rather than translated, with the command that replaced it — `akerdock app logs api`.',
          },
        ],
      },
      {
        id: 'project-file',
        title: 'A `.akerdock` file in your repository',
        blocks: [
          {
            kind: 'p',
            text: 'Drop a `.akerdock` file at the root of the repo and every command in that tree gets its defaults — no UUID to paste, no flags to remember. It never contains a token, so it is committable.',
          },
          {
            kind: 'code',
            caption: '.akerdock',
            code: `context: production
project: varuna
application: api
component: web       # default compose service for logs/shell/port-forward`,
          },
          {
            kind: 'p',
            text: 'Precedence, strongest first: a CLI flag, then an `AKERDOCK_*` variable, then this file, then your global configuration.',
          },
          {
            kind: 'note',
            text: 'Only the **application** has such a default — a repository declares the app it deploys, never the database it talks to — so `akerdock app logs -f` works from that directory with no name, while `db` and `svc` verbs always take one.',
          },
        ],
      },
      {
        id: 'daily',
        title: 'The commands you will actually use',
        blocks: [
          {
            kind: 'code',
            code: `akerdock list                      # apps, databases, stacks
akerdock app logs -f               # the default app of this directory
akerdock app shell                 # a shell in its container
akerdock app info                  # status, health, components, last deploy

akerdock app env set FEATURE_X=on --apply   # write it, and apply it
akerdock app deploy run -f                  # ship, and watch the build
akerdock app deploy rollback                # …or take it back
akerdock app restart                        # start | stop alongside it

akerdock app preview list          # the live PR instances
akerdock app tasks run migrate     # a scheduled task, right now

akerdock db console main           # forward + local client
akerdock db port-forward 15432:5432 main
akerdock db backups run main       # back up before you break something

akerdock tunnel open prod-replica  # a declared external endpoint
akerdock ingress dev 3000          # public URL onto localhost:3000`,
          },
          {
            kind: 'note',
            text: 'The CLI holds your permissions, not more: what the dashboard hides, it refuses too.',
          },
          {
            kind: 'note',
            text: 'Two things stay in the dashboard by decision, not by omission: **restoring a backup** and **approving a fork’s preview**. And a deployment always starts from a source the platform can fetch again — a Git ref or an image — never from a local folder.',
          },
        ],
      },
    ],
  },
  {
    id: 'api',
    title: 'API and tokens',
    icon: 'webhook',
    group: 'Automate',
    summary: 'A personal token, the REST API, and the read-only MCP server.',
    sections: [
      {
        id: 'token',
        title: 'Creating your token',
        blocks: [
          {
            kind: 'p',
            text: 'Under **Personal settings → API tokens**, mint your own token — scoped to the team you are acting in, shown once, stored hashed. A colleague’s tokens are theirs; you never need to share one.',
          },
          {
            kind: 'table',
            head: ['Scope', 'Grants'],
            rows: [
              ['read', 'Reading the inventory and the statuses'],
              ['read:sensitive', 'Reading values normally masked — only if your own role has it'],
              ['write', 'Creating and updating resources'],
              ['deploy', 'Triggering deployments and lifecycle actions'],
              ['root', 'Everything your role can do'],
            ],
          },
          {
            kind: 'warn',
            text: 'A token can never exceed the permissions of the person who created it. Yours narrows with your role — including when your role is narrowed later.',
          },
        ],
      },
      {
        id: 'rest',
        title: 'Calling the API',
        blocks: [
          {
            kind: 'code',
            code: `curl -H "Authorization: Bearer $AKERDOCK_TOKEN" \\
  https://<instance>/api/v1/applications

curl -X POST -H "Authorization: Bearer $AKERDOCK_TOKEN" \\
  https://<instance>/api/v1/applications/<uuid>/deploy`,
          },
          {
            kind: 'p',
            text: 'The contract is an OpenAPI document, and the dashboard itself is one of its clients — anything the UI does, the API does. The API must be enabled at instance level, and can carry an IP allow-list per token.',
          },
        ],
      },
      {
        id: 'mcp',
        title: 'Your assistant, read-only',
        blocks: [
          {
            kind: 'p',
            text: 'When the instance enables it, `akerdock mcp` exposes a read-only MCP server over stdio for a local assistant: overview, servers, projects, applications, databases and services. It reads with your permissions and writes nothing.',
          },
        ],
      },
    ],
  },

  // ── Your account and team ──────────────────────────────────────────────
  {
    id: 'account',
    title: 'Your account',
    icon: 'shield',
    group: 'Your account and team',
    summary: 'Passkeys, two-factor, linked sign-ins and personal tokens.',
    links: [{ label: 'Personal settings', route: '/security' }],
    sections: [
      {
        id: 'auth',
        title: 'Signing in more safely',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**Passkeys** — phishing-resistant, and the least painful option day to day. Enrol one per device.',
              '**Two-factor (TOTP)** — an authenticator app. The instance may require it, in which case every page but Personal settings stays closed until you enrol.',
              '**Linked accounts** — GitHub, GitLab, Google, Microsoft or a company OIDC provider, when the instance offers them.',
            ],
          },
        ],
      },
      {
        id: 'tokens',
        title: 'Personal API tokens',
        blocks: [
          {
            kind: 'p',
            text: 'Created and revoked by you, on the same page. Revoke one the moment a laptop is lost — the value cannot be read again, only replaced.',
          },
        ],
      },
    ],
  },
  {
    id: 'team',
    title: 'Team',
    icon: 'users',
    group: 'Your account and team',
    summary: 'Members, invitations, custom roles and shared variables.',
    permission: 'team:read',
    links: [{ label: 'Members', route: '/team' }, { label: 'Team settings', route: '/settings' }],
    sections: [
      {
        id: 'shared',
        title: 'Shared variables',
        permission: 'secrets:read',
        blocks: [
          {
            kind: 'p',
            text: 'Team settings → **Shared variables** holds the values referenced as `{{team.KEY}}`, `{{project.KEY}}` or `{{environment.KEY}}` from a resource’s variables. Declare a value once at the right scope instead of pasting it into six applications.',
          },
        ],
      },
      {
        id: 'members',
        title: 'Members and invitations',
        permission: 'members:manage',
        blocks: [
          {
            kind: 'p',
            text: 'Invite by email; an invitee with no account creates one from the link itself, at the address the invitation names. A pending invitation can be revoked or its link regenerated.',
          },
        ],
      },
      {
        id: 'roles',
        title: 'Custom roles',
        permission: 'roles:manage',
        blocks: [
          {
            kind: 'p',
            text: 'When admin/member/reviewer do not fit, compose a **custom role** from the permission catalogue and assign it to a member. The three system roles are immutable — deviating means creating a role, not bending an existing one.',
          },
        ],
      },
      {
        id: 'audit',
        title: 'Audit log',
        permission: 'audit:read',
        blocks: [
          {
            kind: 'p',
            text: 'Who did what, on which target, with which result — including tunnel and terminal sessions. It is the first place to look when a resource changed and nobody remembers touching it.',
          },
        ],
      },
    ],
  },

  // ── Instance administration ────────────────────────────────────────────
  {
    id: 'servers',
    title: 'Servers',
    icon: 'server',
    group: 'Instance administration',
    summary: 'Registering a machine, its proxy, its certificates and its cleanup.',
    permission: 'servers:manage',
    links: [{ label: 'Servers', route: '/servers' }],
    sections: [
      {
        id: 'register',
        title: 'Registering and validating',
        blocks: [
          {
            kind: 'steps',
            items: [
              'Add the private key under **Sources → Private keys** (SSH key only — no password, no passphrase).',
              'Register the server with its host, port and user; root, or a non-root user with `sudo NOPASSWD: ALL`.',
              'Run **Validate**: connectivity, prerequisites, Docker Engine, and the health checks.',
              'Set the wildcard domain if new resources should get an automatic subdomain.',
            ],
          },
        ],
      },
      {
        id: 'proxy',
        title: 'Proxy and certificates',
        permission: 'servers:proxy',
        blocks: [
          {
            kind: 'ul',
            items: [
              'Start, stop and restart the proxy, read its logs, edit its configuration. **Stopping it cuts every inbound request on that server.**',
              'The Certificates tab lists what the proxy actually serves, with expiry, and can force a renewal.',
              'Routed domains shows every FQDN the server answers for — the fastest way to find a stale route.',
            ],
          },
        ],
      },
      {
        id: 'maintenance',
        title: 'Cleanup and adoption',
        permission: 'servers:maintain',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**Automated cleanup** reclaims disk on a threshold or a schedule, never during a deployment, and only on resources the platform manages.',
              '**Adoption scans** find containers running on the server that AkerDock does not manage, and adopt them without redeploying — how an existing machine is brought in without downtime.',
            ],
          },
        ],
      },
    ],
  },
  {
    id: 'sources',
    title: 'Sources and credentials',
    icon: 'git-branch',
    group: 'Instance administration',
    summary: 'GitHub Apps, deploy keys, registries, DNS-01 and S3 storages.',
    permission: 'sources:read',
    links: [{ label: 'Sources', route: '/sources' }],
    sections: [
      {
        id: 'families',
        title: 'The five families',
        blocks: [
          {
            kind: 'table',
            head: ['Tab', 'What it unlocks'],
            rows: [
              ['GitHub Apps', 'Private repos, auto-deploy on push, PR previews and status comments'],
              ['Private keys', 'Deploy keys for a private repo, and the SSH keys servers are reached with'],
              ['Registries', 'Pulling from a private registry, and pushing a built image to one'],
              ['DNS credentials', 'Wildcard certificates through the DNS-01 challenge'],
              ['S3 storages', 'Backup destinations — verified before use, and flagged when they stop working'],
            ],
          },
          {
            kind: 'note',
            text: 'Members may add GitHub Apps, registries and S3 storages (`sources:manage`), but never read a private key back: key material needs `keys:reveal`.',
          },
        ],
      },
    ],
  },
  {
    id: 'instance',
    title: 'Instance settings',
    icon: 'settings',
    group: 'Instance administration',
    summary: 'FQDN, email, sign-in providers, API, telemetry and encryption.',
    root: true,
    links: [{ label: 'Global settings', route: '/system' }],
    sections: [
      {
        id: 'tabs',
        title: 'What is configured there',
        blocks: [
          {
            kind: 'ul',
            items: [
              '**Instance** — the FQDN the dashboard is served on and the ACME contact address.',
              '**Teams** — every team on the instance.',
              '**Email** — transactional email (SMTP or Resend) for invitations and password resets; teams may reuse it for their notifications.',
              '**API access** — the API is off until enabled here.',
              '**Sign-in** — OAuth and OIDC providers offered on the login page.',
              '**Telemetry** — remote OTLP export, signal by signal.',
              '**Encryption** — state of encryption at rest and master key rotation.',
              '**Audit** — the instance-wide log, across every team.',
            ],
          },
          {
            kind: 'note',
            text: 'The control plane is never indexable: it serves `robots.txt` with `Disallow: /` and answers `X-Robots-Tag: noindex, nofollow`. That is not a setting.',
          },
        ],
      },
    ],
  },
];
