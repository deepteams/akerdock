import { ChangeDetectionStrategy, Component, effect, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { IconComponent } from '../../../ui/icon/icon.component';
import type { components } from '../../../api/schema';
import type { ConfigForm } from './application-form';

type SourceType = components['schemas']['Application']['source_type'];
type RegistryCredential = components['schemas']['RegistryCredential'];
type PrivateKey = components['schemas']['PrivateKey'];

/** One editable routing row: FQDN(s) sharing a target container port. */
interface RouteRow {
  domains: string;
  port: string;
}

/** The navigable sections of the config form (settings left menu). */
export type ConfigSection =
  'general' | 'source' | 'build' | 'routing' | 'hooks' | 'health' | 'resources';

/** Splits "host/path" into host and (leading-slash) path. */
function splitPath(s: string): { host: string; path: string } {
  const i = s.indexOf('/');
  return i >= 0 ? { host: s.slice(0, i), path: s.slice(i) } : { host: s, path: '' };
}

/** Splits "host:port" into name and port (port only if it is all digits). */
function splitPort(host: string): { name: string; port: string } {
  const i = host.lastIndexOf(':');
  if (i > 0 && /^\d+$/.test(host.slice(i + 1))) {
    return { name: host.slice(0, i), port: host.slice(i + 1) };
  }
  return { name: host, port: '' };
}

/** Parses the newline `domains` string into rows, grouping FQDNs by target
 * port so a shared port reads as one row of comma-separated domains. */
export function parseDomainRows(text: string): RouteRow[] {
  const byPort = new Map<string, string[]>();
  for (const raw of text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)) {
    const { host, path } = splitPath(raw);
    const { name, port } = splitPort(host);
    const list = byPort.get(port) ?? [];
    list.push(name + path);
    byPort.set(port, list);
  }
  return [...byPort.entries()].map(([port, domains]) => ({ domains: domains.join(', '), port }));
}

/** Serializes rows back to the `domains` contract: one `fqdn[:port][/path]`
 * per line. Blank rows and blank domains are dropped. */
export function serializeDomainRows(rows: RouteRow[]): string {
  const lines: string[] = [];
  for (const row of rows) {
    const port = row.port.trim();
    for (const d of row.domains
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)) {
      const { host, path } = splitPath(d);
      lines.push(host + (port ? ':' + port : '') + path);
    }
  }
  return lines.join('\n');
}

/**
 * Every configurable field of an application, in one place: the create page
 * and the settings tab render the SAME fields, so an operator never meets a
 * parameter that exists in one form and not the other.
 *
 * The parent owns the form object; ngModel writes into it directly.
 */
@Component({
  selector: 'app-application-config-fields',
  standalone: true,
  imports: [FormsModule, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (show('general')) {
      <fieldset class="group">
        <legend>General</legend>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-name">Name</label>
          <input
            id="cf-name"
            name="cfName"
            class="akd-input"
            required
            [(ngModel)]="form().name"
            [disabled]="busy()"
          />
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-description">Description</label>
          <input
            id="cf-description"
            name="cfDescription"
            class="akd-input"
            [(ngModel)]="form().description"
            [disabled]="busy()"
          />
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-tags"
            >Tags (comma-separated, usable by the deploy webhook)</label
          >
          <input
            id="cf-tags"
            name="cfTags"
            class="akd-input"
            placeholder="web, prod"
            [(ngModel)]="form().tags"
            [disabled]="busy()"
          />
        </div>
      </fieldset>
    }

    <!-- With a GitHub App the source is the picked repository: URL and deploy
         key would contradict it, so the whole section only exists in manual
         git mode. -->
    @if (show('source') && (sourceType() !== 'git' || !githubApp())) {
      <fieldset class="group">
        <legend>Source · {{ sourceType() }}</legend>
        @switch (sourceType()) {
          @case ('docker_image') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-image"
                >Docker image (registry included if not Docker Hub)</label
              >
              <input
                id="cf-image"
                name="cfImage"
                class="akd-input akd-input--mono"
                placeholder="ghcr.io/acme/app"
                [(ngModel)]="form().dockerImage"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-image-tag">Tag</label>
              <input
                id="cf-image-tag"
                name="cfImageTag"
                class="akd-input akd-input--mono"
                placeholder="latest"
                [(ngModel)]="form().dockerImageTag"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-registry">Private registry credential</label>
              <div class="akd-select">
                <select
                  id="cf-registry"
                  name="cfRegistry"
                  class="akd-input"
                  [(ngModel)]="form().registryCredentialUuid"
                  [disabled]="busy()"
                >
                  <option value="">None (public image)</option>
                  @for (registry of registries(); track registry.uuid) {
                    <option [value]="registry.uuid">
                      {{ registry.name }} ({{ registry.registry_url }})
                    </option>
                  }
                </select>
              </div>
            </div>
          }
          @case ('dockerfile') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-dockerfile">Dockerfile</label>
              <textarea
                id="cf-dockerfile"
                name="cfDockerfile"
                class="akd-textarea akd-mono"
                rows="10"
                placeholder="FROM nginx:alpine&#10;COPY . /usr/share/nginx/html"
                [(ngModel)]="form().dockerfile"
                [disabled]="busy()"
              ></textarea>
            </div>
          }
          @case ('git') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-repo"
                >Repository URL (HTTPS public, or SSH with a deploy key)</label
              >
              <input
                id="cf-repo"
                name="cfRepo"
                class="akd-input akd-input--mono"
                placeholder="https://github.com/acme/app"
                [(ngModel)]="form().gitRepository"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-key">Deploy key (private repositories)</label>
              <div class="akd-select">
                <select
                  id="cf-key"
                  name="cfKey"
                  class="akd-input"
                  [(ngModel)]="form().privateKeyUuid"
                  [disabled]="busy()"
                >
                  <option value="">None (public repository)</option>
                  @for (key of privateKeys(); track key.uuid) {
                    <option [value]="key.uuid">{{ key.name }}</option>
                  }
                </select>
              </div>
            </div>
          }
        }
      </fieldset>
    }

    <!-- Everything about turning the source into an image, GitHub App or not:
         a separate section, so hiding the manual source leaves it in place. -->
    @if (show('build') && sourceType() !== 'docker_image') {
      <fieldset class="group">
        <legend>Build</legend>
        @if (sourceType() === 'git') {
          <div class="akd-field">
            <label class="akd-field__label" for="cf-branch">Branch</label>
            <input
              id="cf-branch"
              name="cfBranch"
              class="akd-input akd-input--mono"
              placeholder="main"
              [(ngModel)]="form().gitBranch"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-buildpack">Build pack</label>
            <div class="akd-select">
              <select
                id="cf-buildpack"
                name="cfBuildPack"
                class="akd-input"
                [(ngModel)]="form().buildPack"
                [disabled]="busy()"
              >
                <option value="" disabled>Choose a build pack…</option>
                <option value="dockerfile">Dockerfile</option>
                <option value="nixpacks">Nixpacks</option>
                <option value="static">Static</option>
                <option value="compose">Docker Compose</option>
              </select>
            </div>
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-basedir">Base directory (monorepos)</label>
            <input
              id="cf-basedir"
              name="cfBaseDir"
              class="akd-input akd-input--mono"
              placeholder="/"
              [(ngModel)]="form().baseDirectory"
              [disabled]="busy()"
            />
          </div>
          @if (form().buildPack === 'dockerfile') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-dockerfile-location"
                >Dockerfile location (relative to base)</label
              >
              <input
                id="cf-dockerfile-location"
                name="cfDockerfileLocation"
                class="akd-input akd-input--mono"
                placeholder="/Dockerfile"
                [(ngModel)]="form().dockerfileLocation"
                [disabled]="busy()"
              />
            </div>
          }
          @if (form().buildPack === 'static') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-publish">Publish directory</label>
              <input
                id="cf-publish"
                name="cfPublish"
                class="akd-input akd-input--mono"
                placeholder="dist"
                [(ngModel)]="form().publishDirectory"
                [disabled]="busy()"
              />
            </div>
          }
          @if (form().buildPack === 'compose') {
            <div class="akd-field">
              <label class="akd-field__label" for="cf-compose-location"
                >Compose file location (relative to base)</label
              >
              <input
                id="cf-compose-location"
                name="cfComposeLocation"
                class="akd-input akd-input--mono"
                placeholder="/docker-compose.yml"
                [(ngModel)]="form().composeFileLocation"
                [disabled]="busy()"
              />
            </div>
            <label class="akd-check">
              <input
                type="checkbox"
                name="cfRawCompose"
                [(ngModel)]="form().rawCompose"
                [disabled]="busy()"
              />
              Raw compose mode (closest to docker compose up — no zero-downtime)
            </label>
          }
          <div class="akd-field">
            <label class="akd-field__label" for="cf-watch"
              >Auto-deploy watch paths (one pattern per line)</label
            >
            <textarea
              id="cf-watch"
              name="cfWatch"
              class="akd-textarea akd-mono"
              rows="2"
              placeholder="apps/shop/**"
              [(ngModel)]="form().watchPaths"
              [disabled]="busy()"
            ></textarea>
          </div>
        }
        <label class="akd-check">
          <input
            type="checkbox"
            name="cfUseBuildServer"
            [(ngModel)]="form().useBuildServer"
            [disabled]="busy()"
          />
          Build on a dedicated build server (requires a push registry)
        </label>
        @if (form().useBuildServer) {
          <div class="akd-field">
            <label class="akd-field__label" for="cf-push-registry">Push registry credential</label>
            <div class="akd-select">
              <select
                id="cf-push-registry"
                name="cfPushRegistry"
                class="akd-input"
                [(ngModel)]="form().pushRegistryCredentialUuid"
                [disabled]="busy()"
              >
                <option value="">Choose a registry…</option>
                @for (registry of registries(); track registry.uuid) {
                  <option [value]="registry.uuid">
                    {{ registry.name }} ({{ registry.registry_url }})
                  </option>
                }
              </select>
            </div>
          </div>
        }
      </fieldset>
    }

    @if (show('routing')) {
      <fieldset class="group">
        <legend>Routing</legend>
        <div class="akd-field">
          <span class="akd-field__label">Domains</span>
          <table class="akd-table routes">
            <thead>
              <tr>
                <th scope="col">Domain(s)</th>
                <th scope="col" class="port-col">Port</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (row of routeRows(); track $index) {
                <tr>
                  <td>
                    <input
                      class="akd-input akd-input--mono"
                      [ngModel]="row.domains"
                      (ngModelChange)="updateRow($index, 'domains', $event)"
                      [ngModelOptions]="{ standalone: true }"
                      [disabled]="busy()"
                      placeholder="app.example.com, www.example.com"
                      aria-label="Domains for this route"
                    />
                  </td>
                  <td class="port-col">
                    <input
                      class="akd-input akd-input--mono"
                      inputmode="numeric"
                      [ngModel]="row.port"
                      (ngModelChange)="updateRow($index, 'port', $event)"
                      [ngModelOptions]="{ standalone: true }"
                      [disabled]="busy()"
                      placeholder="default"
                      aria-label="Target port for this route"
                    />
                  </td>
                  <td class="right">
                    <button
                      class="akd-iconbtn"
                      type="button"
                      [disabled]="busy()"
                      (click)="removeRow($index)"
                      aria-label="Remove route"
                    >
                      <akd-icon name="trash-2" [size]="15" />
                    </button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
          <button
            class="akd-btn akd-btn--secondary akd-btn--sm add-route"
            type="button"
            [disabled]="busy()"
            (click)="addRow()"
          >
            <akd-icon name="plus" [size]="13" />
            Add domain
          </button>
          <span class="akd-field__hint">
            One row per target: one or more FQDNs (comma-separated) routed to the same container
            port. Empty port = the default exposed port. A path is kept (fqdn/path).
          </span>
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-ports"
            >Exposed ports (comma-separated, e.g. 3000)</label
          >
          <input
            id="cf-ports"
            name="cfPorts"
            class="akd-input akd-input--mono"
            [(ngModel)]="form().portsExposes"
            [disabled]="busy()"
          />
        </div>
      </fieldset>
    }

    @if (show('hooks')) {
      <fieldset class="group">
        <legend>Deployment hooks</legend>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-pre">
            Pre-deployment command (runs in the EXISTING container; a failure aborts before any
            mutation)
          </label>
          <input
            id="cf-pre"
            name="cfPre"
            class="akd-input akd-input--mono"
            [(ngModel)]="form().preDeploymentCommand"
            [disabled]="busy()"
          />
        </div>
        <div class="akd-field">
          <label class="akd-field__label" for="cf-post">
            Post-deployment command (runs in the healthy candidate before the switch)
          </label>
          <input
            id="cf-post"
            name="cfPost"
            class="akd-input akd-input--mono"
            [(ngModel)]="form().postDeploymentCommand"
            [disabled]="busy()"
          />
        </div>
      </fieldset>
    }

    @if (show('health')) {
      <fieldset class="group">
        <legend>Health check</legend>
        <label class="akd-check">
          <input
            type="checkbox"
            name="cfHcEnabled"
            [(ngModel)]="form().hcEnabled"
            [disabled]="busy()"
          />
          Enabled (gates routing and zero-downtime switches)
        </label>
        @if (form().hcEnabled) {
          <div class="grid">
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-path">Path</label>
              <input
                id="cf-hc-path"
                name="cfHcPath"
                class="akd-input akd-input--mono"
                placeholder="/"
                [(ngModel)]="form().hcPath"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-port">Port (default: first exposed)</label>
              <input
                id="cf-hc-port"
                name="cfHcPort"
                class="akd-input akd-input--mono"
                inputmode="numeric"
                [(ngModel)]="form().hcPort"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-method">Method</label>
              <input
                id="cf-hc-method"
                name="cfHcMethod"
                class="akd-input akd-input--mono"
                placeholder="GET"
                [(ngModel)]="form().hcMethod"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-interval">Interval (s)</label>
              <input
                id="cf-hc-interval"
                name="cfHcInterval"
                class="akd-input akd-input--mono"
                inputmode="numeric"
                [(ngModel)]="form().hcIntervalSeconds"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-timeout">Timeout (s)</label>
              <input
                id="cf-hc-timeout"
                name="cfHcTimeout"
                class="akd-input akd-input--mono"
                inputmode="numeric"
                [(ngModel)]="form().hcTimeoutSeconds"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-retries">Retries</label>
              <input
                id="cf-hc-retries"
                name="cfHcRetries"
                class="akd-input akd-input--mono"
                inputmode="numeric"
                [(ngModel)]="form().hcRetries"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="cf-hc-start">Start period (s)</label>
              <input
                id="cf-hc-start"
                name="cfHcStart"
                class="akd-input akd-input--mono"
                inputmode="numeric"
                [(ngModel)]="form().hcStartPeriodSeconds"
                [disabled]="busy()"
              />
            </div>
          </div>
        }
      </fieldset>
    }

    @if (show('resources')) {
      <fieldset class="group">
        <legend>Resource limits</legend>
        <div class="grid">
          <div class="akd-field">
            <label class="akd-field__label" for="cf-mem"
              >Memory limit (e.g. 512m, 2g — empty = unlimited)</label
            >
            <input
              id="cf-mem"
              name="cfMem"
              class="akd-input akd-input--mono"
              [(ngModel)]="form().memoryLimit"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-mem-res">Memory reservation</label>
            <input
              id="cf-mem-res"
              name="cfMemRes"
              class="akd-input akd-input--mono"
              [(ngModel)]="form().memoryReservation"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-mem-swap">Memory swap</label>
            <input
              id="cf-mem-swap"
              name="cfMemSwap"
              class="akd-input akd-input--mono"
              [(ngModel)]="form().memorySwap"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-cpu">CPU limit (e.g. 0.5, 2)</label>
            <input
              id="cf-cpu"
              name="cfCpu"
              class="akd-input akd-input--mono"
              [(ngModel)]="form().cpuLimit"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-cpu-shares">CPU shares</label>
            <input
              id="cf-cpu-shares"
              name="cfCpuShares"
              class="akd-input akd-input--mono"
              inputmode="numeric"
              [(ngModel)]="form().cpuShares"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="cf-cpu-set">CPU set (e.g. 0-2)</label>
            <input
              id="cf-cpu-set"
              name="cfCpuSet"
              class="akd-input akd-input--mono"
              [(ngModel)]="form().cpuSet"
              [disabled]="busy()"
            />
          </div>
        </div>
      </fieldset>
    }
  `,
  styles: [
    `
      /* Fieldsets read as kit cards: same surface, border and radius. */
      .group {
        margin: 0 0 var(--space-4);
        padding: var(--space-3) var(--space-4) var(--space-4);
        background: var(--surface-card);
        border: 1px solid var(--border-1);
        border-radius: var(--radius-3);
        display: grid;
        gap: var(--space-3);
      }
      legend {
        padding: 0 var(--space-2);
        font: var(--weight-semibold) var(--text-xs) var(--font-body);
        color: var(--text-3);
        text-transform: uppercase;
        letter-spacing: var(--tracking-wide);
      }
      .grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
        gap: var(--space-3);
      }
      .routes {
        margin-bottom: var(--space-2);
      }
      .routes .port-col {
        width: 8rem;
      }
      .routes .right {
        width: 3rem;
        text-align: right;
      }
      .routes td {
        vertical-align: middle;
      }
      .add-route {
        justify-self: start;
      }
    `,
  ],
})
export class ApplicationConfigFieldsComponent {
  readonly form = input.required<ConfigForm>();
  readonly sourceType = input.required<SourceType>();
  readonly registries = input<RegistryCredential[]>([]);
  readonly privateKeys = input<PrivateKey[]>([]);
  readonly busy = input(false);
  /** True when a GitHub App provides the source: the manual git source
   * section (repository URL, deploy key) is hidden — the Build section
   * (branch, build pack…) stays. */
  readonly githubApp = input(false);
  /** When set, render ONLY this section (the settings tab drives a left menu);
   * undefined renders every section stacked (the create page). */
  readonly section = input<ConfigSection | undefined>(undefined);

  /** The routing table's working state, derived from `form().domains` and
   * serialized back into it on every edit — the parent's contract (a newline
   * string) is untouched. */
  protected readonly routeRows = signal<RouteRow[]>([{ domains: '', port: '' }]);

  constructor() {
    // Re-seed the table when the parent hands a new form object (settings load,
    // a save). Reads the object reference, not `.domains`, so writing back below
    // (a mutation of the same object) does not loop.
    effect(() => {
      const parsed = parseDomainRows(this.form().domains);
      this.routeRows.set(parsed.length > 0 ? parsed : [{ domains: '', port: '' }]);
    });
  }

  protected updateRow(index: number, field: keyof RouteRow, value: string): void {
    const rows = this.routeRows().map((r, i) => (i === index ? { ...r, [field]: value } : r));
    this.routeRows.set(rows);
    this.syncDomains(rows);
  }

  protected addRow(): void {
    this.routeRows.set([...this.routeRows(), { domains: '', port: '' }]);
  }

  protected removeRow(index: number): void {
    const rows = this.routeRows().filter((_, i) => i !== index);
    const next = rows.length > 0 ? rows : [{ domains: '', port: '' }];
    this.routeRows.set(next);
    this.syncDomains(next);
  }

  private syncDomains(rows: RouteRow[]): void {
    this.form().domains = serializeDomainRows(rows);
  }

  protected show(id: ConfigSection): boolean {
    const only = this.section();
    return only === undefined || only === id;
  }
}
