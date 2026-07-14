import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import { ApiError } from '../../api/client';
import type { components } from '../../api/schema';

type Server = components['schemas']['Server'];
type ServerResource = components['schemas']['ServerResource'];
type ServerDomain = components['schemas']['ServerDomain'];
type Certificate = components['schemas']['Certificate'];
type LogLine = components['schemas']['LogLine'];

@Component({
  selector: 'app-server-detail',
  standalone: true,
  imports: [FormsModule, RouterLink, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <div>
          <a routerLink="/servers" class="back">← Servers</a>
          <h1>{{ server()?.name ?? '…' }}</h1>
        </div>
        <button class="akd-btn" type="button" [disabled]="busy()" (click)="validate()">
          Validate
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }
      @if (notice(); as message) {
        <p class="akd-muted" role="status">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (server(); as srv) {
        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Overview</h2>
            <akd-status-badge domain="resource" [state]="srv.status" />
          </header>
          <dl class="akd-dl">
            <dt>SSH</dt>
            <dd class="akd-mono">{{ srv.user }}&#64;{{ srv.host }}:{{ srv.port }}</dd>
            <dt>Architecture</dt>
            <dd>{{ srv.architecture ?? 'unknown until validated' }}</dd>
            <dt>Docker</dt>
            <dd>{{ srv.docker_version ?? 'unknown until validated' }}</dd>
            <dt>Role</dt>
            <dd>{{ srv.is_build_server ? 'dedicated build server' : 'deployment server' }}</dd>
            <dt>Proxy</dt>
            <dd>
              {{ srv.proxy_type ?? 'traefik' }}
              @if (srv.proxy_type !== 'none') {
                (http {{ srv.proxy_http_port ?? 80 }}, https {{ srv.proxy_https_port ?? 443 }})
              }
            </dd>
            <dt>Wildcard domain</dt>
            <dd>{{ srv.wildcard_domain ?? '—' }}</dd>
            <dt>Last observed</dt>
            <dd>{{ srv.observed_at ?? 'never' }}</dd>
          </dl>
        </section>

        @if (srv.proxy_type !== 'none') {
          <section class="akd-card section">
            <header class="akd-bar" style="margin-bottom: 0">
              <h2>Proxy</h2>
              <div class="proxy-actions">
                <button
                  class="akd-btn-ghost"
                  type="button"
                  [disabled]="busy()"
                  (click)="proxy('restart')"
                >
                  Restart
                </button>
                @if (srv.proxy_desired_state === 'stopped') {
                  <button
                    class="akd-btn"
                    type="button"
                    [disabled]="busy()"
                    (click)="proxy('start')"
                  >
                    Start
                  </button>
                } @else {
                  <button
                    class="akd-btn-danger"
                    type="button"
                    [disabled]="busy()"
                    (click)="proxy('stop')"
                  >
                    Stop
                  </button>
                }
              </div>
            </header>

            <dl class="akd-dl">
              <dt>Desired</dt>
              <dd>{{ srv.proxy_desired_state ?? 'running' }}</dd>
              <dt>Observed</dt>
              <dd>
                <akd-status-badge domain="resource" [state]="srv.proxy_observed_status ?? 'unknown'" />
              </dd>
              <dt>Listening</dt>
              <dd class="akd-mono">
                http {{ srv.proxy_http_port ?? 80 }} · https {{ srv.proxy_https_port ?? 443 }}
              </dd>
            </dl>

            @if (srv.proxy_desired_state === 'stopped') {
              <p class="akd-error" role="status">
                The proxy is stopped: every domain routed by this server is down until it is
                started again.
              </p>
            }

            <div class="logs-actions">
              <button
                class="akd-btn-ghost"
                type="button"
                [disabled]="busy()"
                (click)="loadProxyLogs()"
              >
                {{ proxyLogs() === null ? 'Show logs' : 'Refresh logs' }}
              </button>
              @if (proxyLogs() !== null) {
                <button class="akd-btn-ghost" type="button" (click)="proxyLogs.set(null)">
                  Hide
                </button>
              }
            </div>
            @if (proxyLogs(); as lines) {
              <pre class="logs" tabindex="0" aria-label="Proxy logs">@for (l of lines; track l.sequence) {<span class="line">{{ l.message }}</span>}</pre>
            }
          </section>
        }

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Settings</h2>
          </header>
          <p class="akd-muted">
            Changing host, port or user puts the server back in <em>pending</em>: it must be
            validated again before anything deploys to it.
          </p>
          <form class="form" (ngSubmit)="save()">
            <div class="akd-field">
              <label for="sd-name">Name</label>
              <input
                id="sd-name"
                name="name"
                class="akd-input"
                required
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label for="sd-description">Description</label>
              <input
                id="sd-description"
                name="description"
                class="akd-input"
                [(ngModel)]="description"
                [disabled]="busy()"
              />
            </div>
            <div class="row">
              <div class="akd-field grow">
                <label for="sd-host">Host</label>
                <input
                  id="sd-host"
                  name="host"
                  class="akd-input akd-mono"
                  required
                  [(ngModel)]="host"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="sd-port">Port</label>
                <input
                  id="sd-port"
                  name="port"
                  class="akd-input"
                  type="number"
                  min="1"
                  max="65535"
                  [(ngModel)]="port"
                  [disabled]="busy()"
                />
              </div>
              <div class="akd-field">
                <label for="sd-user">User</label>
                <input
                  id="sd-user"
                  name="user"
                  class="akd-input akd-mono"
                  [(ngModel)]="user"
                  [disabled]="busy()"
                />
              </div>
            </div>
            <div>
              <button class="akd-btn" type="submit" [disabled]="busy() || !name.trim()">
                Save settings
              </button>
            </div>
          </form>
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Database CA certificate</h2>
          </header>
          <p class="akd-muted">
            Databases with SSL serve certificates signed by this per-server CA. Clients verify
            against it — a TLS the client does not verify protects nothing.
          </p>
          @if (caCert() === undefined) {
            <div>
              <button class="akd-btn-ghost" type="button" [disabled]="busy()" (click)="loadCA()">
                Show CA certificate
              </button>
            </div>
          } @else if (caCert() === null) {
            <p class="akd-muted">No CA yet — it is generated when the first SSL database is created.</p>
          } @else {
            <pre class="akd-secret ca">{{ caCert() }}</pre>
            <div>
              <button class="akd-btn-ghost" type="button" (click)="downloadCA()">
                Download PEM
              </button>
            </div>
          }
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Resources</h2>
          </header>
          @if (resources().length === 0) {
            <p class="akd-muted">Nothing is deployed on this server.</p>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">Resources deployed on this server</caption>
              <thead>
                <tr>
                  <th scope="col">Kind</th>
                  <th scope="col">Name</th>
                  <th scope="col">Status</th>
                </tr>
              </thead>
              <tbody>
                @for (resource of resources(); track resource.uuid) {
                  <tr>
                    <td class="akd-muted">{{ resource.type }}</td>
                    <td>{{ resource.name }}</td>
                    <td><akd-status-badge domain="resource" [state]="resource.status" /></td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Routed domains</h2>
          </header>
          @if (domains().length === 0) {
            <p class="akd-muted">The proxy routes no domain on this server.</p>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">Domains routed by this server's proxy</caption>
              <thead>
                <tr>
                  <th scope="col">Resource</th>
                  <th scope="col">Domains</th>
                </tr>
              </thead>
              <tbody>
                @for (entry of domains(); track entry.resource_uuid) {
                  <tr>
                    <td class="akd-muted">{{ entry.resource_type }} {{ entry.resource_uuid }}</td>
                    <td class="akd-mono">{{ entry.domains.join(', ') }}</td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </section>

        <section class="akd-card section">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Certificates</h2>
          </header>
          <form class="row" (ngSubmit)="loadCertificates()">
            <div class="akd-field">
              <label for="sd-expiring">Expiring within (days, empty = all)</label>
              <input
                id="sd-expiring"
                name="expiringDays"
                class="akd-input"
                type="number"
                min="0"
                [(ngModel)]="expiringDays"
                [disabled]="busy()"
              />
            </div>
            <button class="akd-btn-ghost" type="submit" [disabled]="busy()">Filter</button>
          </form>
          @if (certificates().length === 0) {
            <p class="akd-muted">No certificate matches.</p>
          } @else {
            <table class="akd-table">
              <caption class="sr-only">TLS certificates served by this server's proxy</caption>
              <thead>
                <tr>
                  <th scope="col">Domain</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Status</th>
                  <th scope="col">Expires</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                @for (cert of certificates(); track cert.uuid) {
                  <tr>
                    <td class="akd-mono">{{ cert.main_domain }}</td>
                    <td class="akd-muted">{{ cert.kind }}</td>
                    <td><akd-status-badge domain="resource" [state]="cert.status" /></td>
                    <td class="akd-muted">{{ cert.not_after ?? '—' }}</td>
                    <td class="right">
                      <button
                        class="akd-btn-ghost"
                        type="button"
                        [attr.aria-expanded]="expandedCert() === cert.uuid"
                        (click)="toggleCert(cert)"
                      >
                        {{ expandedCert() === cert.uuid ? 'Hide' : 'Details' }}
                      </button>
                      <button
                        class="akd-btn-ghost"
                        type="button"
                        [disabled]="busy()"
                        (click)="renew(cert)"
                      >
                        Renew
                      </button>
                    </td>
                  </tr>
                  @if (expandedCert() === cert.uuid && certDetail(); as detail) {
                    <tr>
                      <td colspan="5">
                        <dl class="akd-dl">
                          <dt>Issuer</dt>
                          <dd>{{ detail.issuer ?? '—' }}</dd>
                          <dt>Valid from</dt>
                          <dd>{{ detail.not_before ?? '—' }}</dd>
                          <dt>Valid until</dt>
                          <dd>{{ detail.not_after ?? '—' }}</dd>
                          <dt>SANs</dt>
                          <dd class="akd-mono">
                            {{ detail.sans.length ? detail.sans.join(', ') : '—' }}
                          </dd>
                          @if (detail.last_error) {
                            <dt>Last error</dt>
                            <dd>{{ detail.last_error }}</dd>
                          }
                        </dl>
                      </td>
                    </tr>
                  }
                }
              </tbody>
            </table>
          }
        </section>

        <section class="akd-card section danger-zone">
          <header class="akd-bar" style="margin-bottom: 0">
            <h2>Delete this server</h2>
          </header>
          <p class="akd-muted">
            The server is unregistered from AkerDock. A server still hosting resources cannot be
            deleted.
          </p>
          <div>
            <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove()">
              Delete server
            </button>
          </div>
        </section>
      }
    </div>
  `,
  styles: [
    `
      .back {
        font-size: var(--akd-text-sm);
        color: var(--akd-text-secondary);
        text-decoration: none;
      }
      .back:hover {
        text-decoration: underline;
      }
      .akd-bar h1 {
        margin-top: var(--akd-space-1);
      }
      .section {
        margin-bottom: var(--akd-space-5);
      }
      .form {
        display: grid;
        gap: var(--akd-space-3);
        max-width: 44rem;
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--akd-space-2);
        flex-wrap: wrap;
      }
      .row .grow {
        flex: 1;
        min-width: 14rem;
      }
      .ca {
        margin: 0;
        max-height: 20rem;
        overflow: auto;
        white-space: pre-wrap;
      }
      .danger-zone {
        border-color: var(--akd-status-danger-fg);
      }
          .proxy-actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .logs-actions {
        display: flex;
        gap: var(--akd-space-2);
        margin-top: var(--akd-space-3);
      }
      .logs {
        margin: var(--akd-space-3) 0 0;
        padding: var(--akd-space-3);
        max-height: 24rem;
        overflow: auto;
        background: var(--akd-surface);
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        font-family: var(--akd-font-mono);
        font-size: var(--akd-text-xs);
        line-height: 1.6;
        color: var(--akd-text);
        white-space: pre-wrap;
        word-break: break-word;
      }
      .logs .line {
        display: block;
      }
`,
  ],
})
export class ServerDetailComponent {
  /** Bound from the route (`servers/:uuid`) by withComponentInputBinding. */
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly server = signal<Server | null>(null);
  protected readonly resources = signal<ServerResource[]>([]);
  protected readonly domains = signal<ServerDomain[]>([]);
  protected readonly certificates = signal<Certificate[]>([]);
  /** undefined = not asked yet; null = the server has no CA yet. */
  protected readonly caCert = signal<string | null | undefined>(undefined);
  protected readonly expandedCert = signal<string | null>(null);
  protected readonly certDetail = signal<Certificate | null>(null);
  protected readonly loading = signal(true);
  protected readonly proxyLogs = signal<LogLine[] | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected name = '';
  protected description = '';
  protected host = '';
  protected port = 22;
  protected user = 'root';
  protected expiringDays: number | null = null;

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  /**
   * Proxy lifecycle (§3). Stopping it takes EVERY domain of this server down,
   * so the confirmation says exactly that — the operator is not asked to
   * infer the blast radius from the word "stop".
   */
  protected async proxy(action: 'start' | 'stop' | 'restart'): Promise<void> {
    if (this.busy()) return;
    if (
      action === 'stop' &&
      !confirm(
        'Stop the proxy? Every domain routed by this server stops answering until the proxy is started again.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      await this.api.client().proxyLifecycle(this.uuid(), action);
      this.notice.set(`Proxy ${action} queued — the status updates when the job completes.`);
      await this.load(this.uuid());
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async loadProxyLogs(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const page = await this.api.client().getProxyLogs(this.uuid(), { lines: 200 });
      this.proxyLogs.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async load(uuid: string): Promise<void> {
    this.loading.set(true);
    try {
      const client = this.api.client();
      const [server, resources, domains, certificates] = await Promise.all([
        client.getServer(uuid),
        client.listServerResources(uuid, { limit: 100 }),
        client.listServerDomains(uuid),
        client.listServerCertificates(uuid, { limit: 100 }),
      ]);
      this.setServer(server);
      this.resources.set(resources.data);
      this.domains.set(domains.data);
      this.certificates.set(certificates.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  private setServer(server: Server): void {
    this.server.set(server);
    this.name = server.name;
    this.description = server.description ?? '';
    this.host = server.host;
    this.port = server.port;
    this.user = server.user;
  }

  private async refresh(): Promise<void> {
    try {
      this.setServer(await this.api.client().getServer(this.uuid()));
    } catch {
      // A failed refresh must not wipe what is already on screen.
    }
  }

  /** Validation is a job (202): the UI enqueues it and observes the status. */
  protected async validate(): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const accepted = await this.api.client().validateServer(this.uuid());
      this.notice.set(`Validation queued (job ${accepted.job_uuid}).`);
      await this.refresh();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async save(): Promise<void> {
    const server = this.server();
    if (!server || this.busy() || !this.name.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateServer(this.uuid(), server.version, {
        name: this.name.trim(),
        description: this.description.trim() || null,
        host: this.host.trim(),
        port: this.port,
        user: this.user.trim(),
      });
      this.setServer(updated);
      this.notice.set('Settings saved.');
    } catch (err) {
      // A 409 version conflict means someone else changed the server while this
      // form was open: reload their version instead of clobbering it (§24.1).
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.refresh();
        this.error.set(
          'Your edit raced a concurrent change: the latest configuration was reloaded. Re-apply your edit on top of it.',
        );
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }

  protected async loadCA(): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      const { ca_cert } = await this.api.client().getServerCA(this.uuid());
      this.caCert.set(ca_cert);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  /** The PEM is public material — offer it as a file, clients mount it as-is. */
  protected downloadCA(): void {
    const pem = this.caCert();
    if (!pem) return;
    const url = URL.createObjectURL(new Blob([pem], { type: 'application/x-pem-file' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = `akerdock-server-${this.uuid()}-ca.pem`;
    link.click();
    URL.revokeObjectURL(url);
  }

  protected async loadCertificates(): Promise<void> {
    this.error.set(null);
    try {
      const page = await this.api.client().listServerCertificates(this.uuid(), {
        limit: 100,
        expiring_within_days: this.expiringDays ?? undefined,
      });
      this.certificates.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async toggleCert(cert: Certificate): Promise<void> {
    if (this.expandedCert() === cert.uuid) {
      this.expandedCert.set(null);
      return;
    }
    this.expandedCert.set(cert.uuid);
    this.certDetail.set(null);
    try {
      this.certDetail.set(await this.api.client().getCertificate(cert.uuid));
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async renew(cert: Certificate): Promise<void> {
    if (
      !confirm(
        `Renew the certificate for "${cert.main_domain}" now? A new ACME issuance is attempted — repeated renewals can hit the CA's rate limits.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const accepted = await this.api.client().renewCertificate(cert.uuid);
      this.notice.set(`Renewal queued (job ${accepted.job_uuid}).`);
      await this.loadCertificates();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    const server = this.server();
    if (!server) return;
    if (
      !confirm(
        `Delete the server "${server.name}"? It is unregistered from AkerDock; nothing is uninstalled from the machine itself.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteServer(this.uuid());
      await this.router.navigateByUrl('/servers');
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }
}
