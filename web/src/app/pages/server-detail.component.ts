import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { TerminalComponent } from '../../ui/terminal/terminal.component';
import type { TerminalSessionInfo } from '../../ui/terminal/protocol';
import { ApiService } from '../core/api.service';
import { ApiError } from '../../api/client';
import type { components } from '../../api/schema';

type Server = components['schemas']['Server'];
type ServerResource = components['schemas']['ServerResource'];
type ServerDomain = components['schemas']['ServerDomain'];
type Certificate = components['schemas']['Certificate'];
type LogLine = components['schemas']['LogLine'];
type DnsCredential = components['schemas']['DnsCredential'];

/** A certificate this close to its expiry is a renewal that should already have happened. */
const EXPIRY_WARN_DAYS = 14;

@Component({
  selector: 'app-server-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    StatusBadgeComponent,
    CardComponent,
    IconComponent,
    TerminalComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <a
          class="akd-iconbtn akd-iconbtn--bordered"
          routerLink="/servers"
          aria-label="Back to servers"
        >
          <akd-icon name="arrow-left" [size]="15" />
        </a>
        <h1 class="title-mono">{{ server()?.name ?? '…' }}</h1>
        @if (server(); as srv) {
          <akd-status-badge domain="resource" [state]="srv.status" />
          <span class="akd-mono faint">{{ srv.user }}&#64;{{ srv.host }}:{{ srv.port }}</span>
        }
        <span class="grow"></span>
        <button
          class="akd-btn akd-btn--secondary"
          type="button"
          [disabled]="busy()"
          (click)="validate()"
        >
          <akd-icon name="check-circle" [size]="15" />
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
        <!-- pending/unreachable are diagnoses, not just states: say what each
             means and where the remediation message lives (the validate job
             records one per step, §20.1). -->
        @if (srv.status === 'pending') {
          <div class="valhint valhint--warn" role="status">
            <akd-icon name="alert-triangle" [size]="15" />
            <span class="valhint__text">
              SSH reaches this server, but validation stopped at the system checks — unsupported OS,
              or Docker missing / too old / unreachable over a non-interactive shell. The failing
              step and its remediation are recorded on the
              @if (validationJobUuid(); as job) {
                <a [routerLink]="['/jobs', job]">validation job</a>
              } @else {
                <a routerLink="/jobs" [queryParams]="{ type: 'server.validate' }">validation job</a>
              }
              .
            </span>
          </div>
        } @else if (srv.status === 'unreachable') {
          <div class="valhint valhint--danger" role="status">
            <akd-icon name="alert-triangle" [size]="15" />
            <span class="valhint__text">
              SSH connection failed — check host, port, user, firewall, and that the public key is
              in authorized_keys. Details on the
              @if (validationJobUuid(); as job) {
                <a [routerLink]="['/jobs', job]">validation job</a>
              } @else {
                <a routerLink="/jobs" [queryParams]="{ type: 'server.validate' }">validation job</a>
              }
              .
            </span>
          </div>
        }
        <div class="stack">
          <!-- Two very different "stopped": a proxy that has never run yet is
               a setup step, not an incident. Only a proxy that DID serve
               traffic gets the alarm treatment. -->
          @if (
            !srv.is_build_server &&
            srv.proxy_type !== 'none' &&
            srv.proxy_desired_state === 'stopped'
          ) {
            @if ((srv.proxy_observed_status ?? 'unknown') === 'unknown') {
              <div class="notstarted-banner" role="status">
                <akd-icon name="info" [size]="15" />
                <span>
                  The proxy has not been started yet — nothing listens on ports
                  {{ srv.proxy_http_port ?? 80 }}/{{ srv.proxy_https_port ?? 443 }} until you review
                  the proxy settings below and press Start. The first start creates the
                  configuration and the container.
                </span>
                <span class="grow"></span>
                <button
                  class="akd-btn akd-btn--primary akd-btn--sm"
                  type="button"
                  [disabled]="busy()"
                  (click)="proxy('start')"
                >
                  <akd-icon name="play" [size]="14" />
                  Start proxy
                </button>
              </div>
            } @else {
              <div class="stopped-banner" role="status">
                The proxy is intentionally stopped — every domain routed by this server is down
                until it is started again. Drift reconciliation will not restart it.
                <span class="grow"></span>
                <button
                  class="akd-btn akd-btn--primary akd-btn--sm"
                  type="button"
                  [disabled]="busy()"
                  (click)="proxy('start')"
                >
                  <akd-icon name="play" [size]="14" />
                  Start proxy
                </button>
              </div>
            }
          }

          <akd-card title="Overview">
            <dl class="akd-dl">
              <dt>Architecture</dt>
              <dd>{{ srv.architecture ?? 'unknown until validated' }}</dd>
              <dt>Docker</dt>
              <dd>{{ srv.docker_version ?? 'unknown until validated' }}</dd>
              <dt>Role</dt>
              <dd>{{ srv.is_build_server ? 'dedicated build server' : 'deployment server' }}</dd>
              <dt>Wildcard domain</dt>
              <dd>{{ srv.wildcard_domain ?? '—' }}</dd>
              <dt>Last observed</dt>
              <dd>{{ srv.observed_at ?? 'never' }}</dd>
              <dt>Agent</dt>
              <dd>
                @if (srv.agent_connected) {
                  <akd-status-badge domain="resource" state="healthy" label="connected" />
                } @else if (srv.agent_seen_at) {
                  <akd-status-badge
                    domain="resource"
                    state="unknown"
                    [label]="'silent — last seen ' + srv.agent_seen_at"
                  />
                } @else {
                  <span class="akd-badge akd-badge--mono">not enrolled</span>
                }
              </dd>
            </dl>
          </akd-card>

          <!-- A build server hosts no application, so it routes nothing (§3.4):
               the proxy card would offer controls the backend refuses. -->
          @if (!srv.is_build_server && srv.proxy_type !== 'none') {
            <akd-card title="Proxy">
              <span card-actions class="badges">
                <akd-status-badge
                  domain="resource"
                  [state]="srv.proxy_desired_state ?? 'running'"
                  [label]="'desired: ' + (srv.proxy_desired_state ?? 'running')"
                />
                <akd-status-badge
                  domain="resource"
                  [state]="srv.proxy_observed_status ?? 'unknown'"
                  [label]="'observed: ' + (srv.proxy_observed_status ?? 'unknown')"
                />
              </span>
              <div class="proxy-body">
                <div class="proxy-meta">
                  {{ srv.proxy_type ?? 'traefik' }} · listening on
                  <span class="akd-mono">
                    :{{ srv.proxy_http_port ?? 80 }} :{{ srv.proxy_https_port ?? 443 }}
                  </span>
                </div>
                <div class="proxy-actions">
                  @if (srv.proxy_desired_state === 'stopped') {
                    <button
                      class="akd-btn akd-btn--primary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="proxy('start')"
                    >
                      <akd-icon name="play" [size]="14" />
                      Start
                    </button>
                  } @else {
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="proxy('stop')"
                    >
                      <akd-icon name="square" [size]="14" />
                      Stop
                    </button>
                  }
                  <!-- Restart drives an existing container: before the first
                       start there is nothing to restart. -->
                  @if ((srv.proxy_observed_status ?? 'unknown') !== 'unknown') {
                    <button
                      class="akd-btn akd-btn--secondary akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="proxy('restart')"
                    >
                      <akd-icon name="refresh-cw" [size]="14" />
                      Restart
                    </button>
                  }
                  <button
                    class="akd-btn akd-btn--ghost akd-btn--sm"
                    type="button"
                    [disabled]="busy()"
                    (click)="loadProxyLogs()"
                  >
                    <akd-icon name="logs" [size]="14" />
                    {{ proxyLogs() === null ? 'Proxy logs' : 'Refresh logs' }}
                  </button>
                  @if (proxyLogs() !== null) {
                    <button
                      class="akd-btn akd-btn--ghost akd-btn--sm"
                      type="button"
                      (click)="proxyLogs.set(null)"
                    >
                      Hide
                    </button>
                  }
                </div>
                @if (proxyLogs(); as lines) {
                  <div class="akd-log logs" tabindex="0" aria-label="Proxy logs">
                    @for (line of lines; track line.sequence) {
                      <div class="akd-log__line">
                        <span class="akd-log__ts">{{ ts(line) }}</span>
                        <span class="akd-log__msg">{{ line.message }}</span>
                      </div>
                    }
                  </div>
                }
              </div>
            </akd-card>
          }

          @if (!srv.is_build_server) {
            <akd-card title="Proxy settings">
              <form class="proxyform" (ngSubmit)="saveProxySettings()">
                <div class="proxyform__grid">
                  <div class="akd-field">
                    <label class="akd-field__label" for="sd-proxy-type">Proxy</label>
                    <div class="akd-select">
                      <select
                        id="sd-proxy-type"
                        name="proxyType"
                        class="akd-input"
                        [(ngModel)]="proxyType"
                        [disabled]="busy()"
                      >
                        <option value="traefik">traefik (managed)</option>
                        <option value="none">none — this server routes nothing</option>
                      </select>
                    </div>
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="sd-proxy-http">HTTP port</label>
                    <input
                      id="sd-proxy-http"
                      name="proxyHttpPort"
                      class="akd-input akd-input--mono"
                      type="number"
                      min="1"
                      max="65535"
                      [(ngModel)]="proxyHttpPort"
                      [disabled]="busy() || proxyType === 'none'"
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="sd-proxy-https">HTTPS port</label>
                    <input
                      id="sd-proxy-https"
                      name="proxyHttpsPort"
                      class="akd-input akd-input--mono"
                      type="number"
                      min="1"
                      max="65535"
                      [(ngModel)]="proxyHttpsPort"
                      [disabled]="busy() || proxyType === 'none'"
                    />
                  </div>
                </div>
                <div class="proxyform__grid">
                  <div class="akd-field">
                    <label class="akd-field__label" for="sd-wildcard">Wildcard domain</label>
                    <input
                      id="sd-wildcard"
                      name="wildcardDomain"
                      class="akd-input akd-input--mono"
                      placeholder="*.apps.example.com"
                      [(ngModel)]="wildcardDomain"
                      [disabled]="busy() || proxyType === 'none'"
                    />
                    <span class="akd-field__hint">
                      With a DNS-01 credential: one wildcard certificate covers every host. Without:
                      the wildcard is only a naming template — each assigned host gets its own
                      HTTP-01 certificate (hosts must be publicly reachable on the HTTP port; CA
                      rate limits apply per host).
                    </span>
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="sd-dns">DNS-01 credential</label>
                    <div class="akd-select">
                      <select
                        id="sd-dns"
                        name="dnsCredentialUuid"
                        class="akd-input"
                        [(ngModel)]="dnsCredentialUuid"
                        [disabled]="busy() || proxyType === 'none'"
                      >
                        <option value="">none</option>
                        @for (cred of dnsCredentials(); track cred.uuid) {
                          <option [value]="cred.uuid">{{ cred.name }} ({{ cred.provider }})</option>
                        }
                      </select>
                    </div>
                  </div>
                </div>
                <div class="proxyform__actions">
                  <span class="akd-field__hint">
                    Applied when the proxy container is (re)created — for a proxy not started yet,
                    at its first start. The ACME contact email lives in Global settings.
                  </span>
                  <button class="akd-btn akd-btn--secondary" type="submit" [disabled]="busy()">
                    Save proxy settings
                  </button>
                </div>
              </form>
            </akd-card>
          }

          <akd-card title="Certificates" [padded]="false">
            <form card-actions class="cert-filter" (ngSubmit)="loadCertificates()">
              @if (expiringCount() > 0) {
                <span class="akd-badge akd-badge--warn akd-badge--mono">
                  {{ expiringCount() }} expiring
                </span>
              }
              <label class="sr-only" for="sd-expiring">Expiring within (days, empty = all)</label>
              <input
                id="sd-expiring"
                name="expiringDays"
                class="akd-input akd-input--mono days"
                type="number"
                min="0"
                placeholder="days"
                [(ngModel)]="expiringDays"
                [disabled]="busy()"
              />
              <button class="akd-btn akd-btn--ghost akd-btn--sm" type="submit" [disabled]="busy()">
                Filter
              </button>
            </form>
            @if (certificates().length === 0) {
              <p class="akd-muted pad">No certificate matches.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  TLS certificates served by this server's proxy
                </caption>
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
                      <td>
                        <span class="akd-badge akd-badge--mono">{{ cert.kind }}</span>
                      </td>
                      <td><akd-status-badge domain="resource" [state]="cert.status" /></td>
                      <td>
                        @if (cert.not_after) {
                          <span
                            class="akd-badge akd-badge--mono"
                            [class.akd-badge--warn]="expiringSoon(cert)"
                          >
                            {{ expiry(cert) }}
                          </span>
                        } @else {
                          <span class="akd-muted">—</span>
                        }
                      </td>
                      <td class="right">
                        <button
                          class="akd-btn akd-btn--ghost akd-btn--sm"
                          type="button"
                          [attr.aria-expanded]="expandedCert() === cert.uuid"
                          (click)="toggleCert(cert)"
                        >
                          {{ expandedCert() === cert.uuid ? 'Hide' : 'Details' }}
                        </button>
                        <button
                          class="akd-btn akd-btn--ghost akd-btn--sm"
                          type="button"
                          [disabled]="busy()"
                          (click)="renew(cert)"
                        >
                          <akd-icon name="refresh-cw" [size]="14" />
                          Renew
                        </button>
                      </td>
                    </tr>
                    @if (expandedCert() === cert.uuid && certDetail(); as detail) {
                      <tr>
                        <td colspan="5">
                          <dl class="akd-dl cert-detail">
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
          </akd-card>

          <akd-card title="Resources on this server" [padded]="false">
            @if (resources().length === 0) {
              <p class="akd-muted pad">Nothing is deployed on this server.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  Resources deployed on this server
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Resource</th>
                    <th scope="col">Kind</th>
                    <th scope="col">Status</th>
                  </tr>
                </thead>
                <tbody>
                  @for (resource of resources(); track resource.uuid) {
                    <tr>
                      <td class="akd-mono">{{ resource.name }}</td>
                      <td>
                        <span class="akd-badge akd-badge--mono">{{ resource.type }}</span>
                      </td>
                      <td><akd-status-badge domain="resource" [state]="resource.status" /></td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          </akd-card>

          <akd-card title="Routed domains" [padded]="false">
            @if (domains().length === 0) {
              <p class="akd-muted pad">The proxy routes no domain on this server.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  Domains routed by this server's proxy
                </caption>
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
          </akd-card>

          <akd-card title="Settings">
            <p class="akd-muted intro">
              Changing host, port or user puts the server back in <em>pending</em>: it must be
              validated again before anything deploys to it.
            </p>
            <form class="form" (ngSubmit)="save()">
              <div class="akd-field">
                <label class="akd-field__label" for="sd-name">Name</label>
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
                <label class="akd-field__label" for="sd-description">Description</label>
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
                  <label class="akd-field__label" for="sd-host">Host</label>
                  <input
                    id="sd-host"
                    name="host"
                    class="akd-input akd-input--mono"
                    required
                    [(ngModel)]="host"
                    [disabled]="busy()"
                  />
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="sd-port">Port</label>
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
                  <label class="akd-field__label" for="sd-user">User</label>
                  <input
                    id="sd-user"
                    name="user"
                    class="akd-input akd-input--mono"
                    [(ngModel)]="user"
                    [disabled]="busy()"
                  />
                </div>
              </div>
              <div>
                <button
                  class="akd-btn akd-btn--primary"
                  type="submit"
                  [disabled]="busy() || !name.trim()"
                >
                  Save settings
                </button>
              </div>
            </form>
          </akd-card>

          <akd-card title="Automated cleanup">
            <p class="akd-muted intro">
              Reclaims the Docker build cache, dangling images and dead deployment candidates on a
              schedule or when the disk crosses a threshold (§3.7). It is deferred while a
              deployment is running, and never touches tagged images (rollback artifacts) or named
              volumes.
            </p>
            <form class="form" (ngSubmit)="saveCleanup()">
              <div class="akd-field">
                <label class="akd-check">
                  <input
                    type="checkbox"
                    name="cleanupEnabled"
                    [(ngModel)]="cleanupEnabled"
                    [disabled]="busy()"
                  />
                  Enable automated cleanup
                </label>
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="sd-cleanup-threshold"
                  >Disk usage threshold (%)</label
                >
                <input
                  id="sd-cleanup-threshold"
                  name="cleanupThreshold"
                  class="akd-input akd-input--mono"
                  type="number"
                  min="1"
                  max="100"
                  placeholder="e.g. 70"
                  [(ngModel)]="cleanupDiskThreshold"
                  [disabled]="busy() || !cleanupEnabled"
                />
                <span class="akd-field__hint">
                  Runs a cleanup when disk usage crosses this percentage. Empty = no threshold
                  trigger.
                </span>
              </div>
              <div class="akd-field">
                <label class="akd-field__label" for="sd-cleanup-cron">Schedule (cron)</label>
                <input
                  id="sd-cleanup-cron"
                  name="cleanupCron"
                  class="akd-input akd-input--mono"
                  placeholder="daily"
                  [(ngModel)]="cleanupCron"
                  [disabled]="busy() || !cleanupEnabled"
                />
                <span class="akd-field__hint">
                  A 5-field cron expression, or a preset: hourly, daily, weekly, monthly. Empty = no
                  scheduled run. Set a threshold and/or a schedule — at least one is needed to fire.
                </span>
              </div>
              <div class="akd-field">
                <label class="akd-check">
                  <input
                    type="checkbox"
                    name="cleanupPruneVolumes"
                    [(ngModel)]="cleanupPruneVolumes"
                    [disabled]="busy() || !cleanupEnabled"
                  />
                  Also prune anonymous volumes (named and data volumes are never touched)
                </label>
              </div>
              <div class="akd-field">
                <label class="akd-check">
                  <input
                    type="checkbox"
                    name="cleanupPruneNetworks"
                    [(ngModel)]="cleanupPruneNetworks"
                    [disabled]="busy() || !cleanupEnabled"
                  />
                  Also prune unused managed networks
                </label>
              </div>
              @if (cleanupLastRunAt(); as last) {
                <p class="akd-muted sm">Last cleanup: {{ last }}</p>
              }
              <div class="cleanup-actions">
                <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                  Save cleanup
                </button>
                <button
                  class="akd-btn akd-btn--secondary"
                  type="button"
                  [disabled]="busy()"
                  (click)="runCleanup()"
                >
                  Run cleanup now
                </button>
              </div>
            </form>
          </akd-card>

          <akd-card title="Database CA certificate">
            <p class="akd-muted intro">
              Databases with SSL serve certificates signed by this per-server CA. Clients verify
              against it — a TLS the client does not verify protects nothing.
            </p>
            @if (caCert() === undefined) {
              <div>
                <button
                  class="akd-btn akd-btn--secondary"
                  type="button"
                  [disabled]="busy()"
                  (click)="loadCA()"
                >
                  <akd-icon name="eye" [size]="15" />
                  Show CA certificate
                </button>
              </div>
            } @else if (caCert() === null) {
              <p class="akd-muted">
                No CA yet — it is generated when the first SSL database is created.
              </p>
            } @else {
              <pre class="akd-secret ca">{{ caCert() }}</pre>
              <div>
                <button class="akd-btn akd-btn--secondary" type="button" (click)="downloadCA()">
                  <akd-icon name="download" [size]="15" />
                  Download PEM
                </button>
              </div>
            }
          </akd-card>

          <akd-card>
            <akd-terminal
              title="Server shell"
              hint="Opens a root shell on this server over SSH — the blast radius is the whole machine, every application and every database on it. Re-authentication with a passkey is required, and the session is audited."
              [open]="openTerminal"
            />
          </akd-card>

          <div class="akd-card danger-zone">
            <div class="akd-card__header">
              <h2 class="akd-card__title">Delete this server</h2>
            </div>
            <div class="akd-card__body">
              <p class="akd-muted intro">
                The server is unregistered from AkerDock. A server still hosting resources cannot be
                deleted.
              </p>
              <div>
                <button
                  class="akd-btn akd-btn--danger"
                  type="button"
                  [disabled]="busy()"
                  (click)="remove()"
                >
                  <akd-icon name="trash-2" [size]="15" />
                  Delete server
                </button>
              </div>
            </div>
          </div>
        </div>
      }
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .title-mono {
        font-family: var(--font-mono);
      }
      .faint {
        color: var(--text-faint);
      }
      .stack {
        display: grid;
        gap: var(--space-5);
      }
      .stopped-banner {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-2) var(--space-4);
        font-size: var(--text-sm);
        color: var(--danger);
        background: var(--danger-dim);
        border: 1px solid var(--danger-border);
        border-radius: var(--radius-2);
      }
      .notstarted-banner {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-2) var(--space-4);
        font-size: var(--text-sm);
        color: var(--accent);
        background: var(--accent-dim);
        border: 1px solid var(--accent-border);
        border-radius: var(--radius-2);
      }
      .notstarted-banner span:first-of-type {
        color: var(--text-2);
      }
      .proxyform {
        display: grid;
        gap: var(--space-4);
      }
      .proxyform__grid {
        display: grid;
        grid-template-columns: 2fr 1fr 1fr;
        gap: var(--space-4);
        align-items: start;
      }
      .proxyform__grid:nth-of-type(2) {
        grid-template-columns: 2fr 2fr;
      }
      .proxyform__actions {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-4);
      }
      .badges {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
      }
      .valhint {
        display: flex;
        align-items: flex-start;
        gap: var(--space-3);
        padding: 10px 16px;
        border-radius: var(--radius-2);
        font-size: var(--text-sm);
        margin-bottom: var(--space-4);
      }
      .valhint--warn {
        background: var(--warn-dim);
        border: 1px solid var(--warn-border);
        color: var(--warn);
      }
      .valhint--danger {
        background: var(--danger-dim);
        border: 1px solid var(--danger-border);
        color: var(--danger);
      }
      .valhint__text {
        color: var(--text-2);
      }
      .valhint__text a {
        color: inherit;
        text-decoration: underline;
      }
      .proxy-body {
        display: grid;
        gap: var(--space-3);
      }
      .proxy-meta {
        font-size: var(--text-sm);
        color: var(--text-2);
      }
      .proxy-actions {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .logs {
        max-height: 24rem;
        padding: var(--space-2) 0;
      }
      .cert-filter {
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .cert-filter .days {
        width: 6rem;
      }
      .cert-detail {
        padding: var(--space-3) 0;
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .intro {
        margin: 0 0 var(--space-4);
        font-size: var(--text-sm);
      }
      .form {
        display: grid;
        gap: var(--space-4);
        max-width: 44rem;
      }
      .cleanup-actions {
        display: flex;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--space-3);
        flex-wrap: wrap;
      }
      .row .grow {
        flex: 1;
        min-width: 14rem;
      }
      .ca {
        margin: 0 0 var(--space-4);
        max-height: 20rem;
        overflow: auto;
        white-space: pre-wrap;
      }
      .danger-zone {
        border-color: var(--danger-border);
      }
      akd-card {
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
  /** UUID of the validation job this session enqueued — the diagnosis lives in its steps. */
  protected readonly validationJobUuid = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected readonly expiringCount = computed(
    () => this.certificates().filter((cert) => this.expiringSoon(cert)).length,
  );

  /**
   * Opens a server shell — a root terminal, so the server demands a fresh
   * passkey re-authentication (rbac-matrix §5). Rather than asking for it up
   * front, we let the API say when it is needed (`stepup_required`), do the
   * ceremony, and retry once: the step-up is then never demanded when the
   * session is already fresh enough.
   */
  protected readonly openTerminal = async (): Promise<TerminalSessionInfo> => {
    try {
      return (await this.api
        .client()
        .createServerTerminalSession(this.uuid())) as unknown as TerminalSessionInfo;
    } catch (err) {
      if (!(err instanceof ApiError) || err.code !== 'stepup_required') throw err;
      await this.api.stepUpWithPasskey();
      return (await this.api
        .client()
        .createServerTerminalSession(this.uuid())) as unknown as TerminalSessionInfo;
    }
  };

  protected name = '';
  protected description = '';
  protected host = '';
  protected port = 22;
  protected user = 'root';
  protected expiringDays: number | null = null;

  protected readonly dnsCredentials = signal<DnsCredential[]>([]);
  protected proxyType: 'traefik' | 'none' = 'traefik';
  protected proxyHttpPort = 80;
  protected proxyHttpsPort = 443;
  protected wildcardDomain = '';
  protected dnsCredentialUuid = '';

  // Automated Docker cleanup (§3.7): reclaims build cache, dangling images and
  // dead candidates so a busy server does not fill its disk.
  protected cleanupEnabled = false;
  protected cleanupDiskThreshold: number | null = null;
  protected cleanupCron = '';
  protected cleanupPruneVolumes = false;
  protected cleanupPruneNetworks = false;
  protected readonly cleanupLastRunAt = signal<string | null>(null);

  constructor() {
    effect(() => {
      const uuid = this.uuid();
      untracked(() => void this.load(uuid));
    });
  }

  protected ts(line: LogLine): string {
    return new Date(line.timestamp).toLocaleTimeString();
  }

  protected expiry(cert: Certificate): string {
    return cert.not_after ? cert.not_after.slice(0, 10) : '—';
  }

  protected expiringSoon(cert: Certificate): boolean {
    if (!cert.not_after) return false;
    const remaining = Date.parse(cert.not_after) - Date.now();
    return remaining < EXPIRY_WARN_DAYS * 24 * 60 * 60 * 1000;
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
      // Feeds the DNS-01 select of the proxy settings; decorative, must not
      // block the page.
      try {
        const creds = await client.listDnsCredentials({ limit: 100 });
        this.dnsCredentials.set(creds.data);
      } catch {
        /* the select simply stays empty */
      }
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
    this.proxyType = (server.proxy_type as 'traefik' | 'none' | undefined) ?? 'traefik';
    this.proxyHttpPort = server.proxy_http_port ?? 80;
    this.proxyHttpsPort = server.proxy_https_port ?? 443;
    this.wildcardDomain = server.wildcard_domain ?? '';
    this.dnsCredentialUuid = server.dns_credential_uuid ?? '';
    this.cleanupEnabled = server.cleanup_enabled ?? false;
    this.cleanupDiskThreshold = server.cleanup_disk_threshold_pct ?? null;
    this.cleanupCron = server.cleanup_cron ?? '';
    this.cleanupPruneVolumes = server.cleanup_prune_volumes ?? false;
    this.cleanupPruneNetworks = server.cleanup_prune_networks ?? false;
    this.cleanupLastRunAt.set(server.cleanup_last_run_at ?? null);
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
      this.validationJobUuid.set(accepted.job_uuid ?? null);
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

  protected async saveProxySettings(): Promise<void> {
    const server = this.server();
    if (!server || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateServer(this.uuid(), server.version, {
        proxy_type: this.proxyType,
        proxy_http_port: this.proxyHttpPort,
        proxy_https_port: this.proxyHttpsPort,
        wildcard_domain: this.wildcardDomain.trim() || null,
        dns_credential_uuid: this.dnsCredentialUuid || null,
      });
      this.setServer(updated);
      this.notice.set(
        'Proxy settings saved — they take effect when the proxy container is (re)created.',
      );
    } catch (err) {
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

  protected async saveCleanup(): Promise<void> {
    const server = this.server();
    if (!server || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const threshold =
        this.cleanupDiskThreshold != null && this.cleanupDiskThreshold > 0
          ? Math.min(100, Math.max(1, Math.trunc(this.cleanupDiskThreshold)))
          : null;
      const updated = await this.api.client().updateServer(this.uuid(), server.version, {
        cleanup_enabled: this.cleanupEnabled,
        cleanup_disk_threshold_pct: threshold,
        cleanup_cron: this.cleanupCron.trim() || null,
        cleanup_prune_volumes: this.cleanupPruneVolumes,
        cleanup_prune_networks: this.cleanupPruneNetworks,
      });
      this.setServer(updated);
      this.notice.set(
        this.cleanupEnabled
          ? 'Automated cleanup saved.'
          : 'Automated cleanup disabled — the server disk is no longer reclaimed automatically.',
      );
    } catch (err) {
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

  /** One-off cleanup (202): enqueues the same job the schedule fires. */
  protected async runCleanup(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      await this.api.client().runServerCleanup(this.uuid());
      this.notice.set(
        'Cleanup queued — build cache, dangling images and dead candidates are reclaimed.',
      );
    } catch (err) {
      this.error.set(ApiService.describe(err));
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
