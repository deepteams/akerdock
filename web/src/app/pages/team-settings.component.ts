import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { AuditFetch, AuditLogComponent } from './audit-log.component';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import type { components } from '../../api/schema';

type SharedVariable = components['schemas']['SharedVariable'];
type Scope = SharedVariable['scope'];
type Team = components['schemas']['Team'];
type ScimToken = components['schemas']['ScimToken'];
type ScimTokenCreated = components['schemas']['ScimTokenCreated'];
type SettingsTab = 'variables' | 'config' | 'audit' | 'provisioning';

/**
 * Team-level settings. Two tabs: Variables — the team-scoped shared variables
 * (`{{team.KEY}}` resolved at deploy time), edited inline in a table; and
 * Config — the team's name and description. Connections live on the
 * Notifications page; there is no team deletion (the API exposes none).
 */
@Component({
  selector: 'app-team-settings',
  standalone: true,
  imports: [FormsModule, SlicePipe, AuditLogComponent, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Team settings</h1>
        @if (team()?.name) {
          <span class="akd-badge akd-badge--mono">{{ team()?.name }}</span>
        }
      </header>

      <nav class="akd-tabs" role="tablist" aria-label="Team settings sections">
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="active() === 'variables'"
          [attr.aria-selected]="active() === 'variables'"
          (click)="active.set('variables')"
        >
          Variables
          @if (variables().length > 0) {
            <span class="akd-tab__count">{{ variables().length }}</span>
          }
        </button>
        <button
          type="button"
          class="akd-tab"
          role="tab"
          [class.akd-tab--active]="active() === 'config'"
          [attr.aria-selected]="active() === 'config'"
          (click)="active.set('config')"
        >
          Config
        </button>
        @if (canAudit()) {
          <button
            type="button"
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="active() === 'audit'"
            [attr.aria-selected]="active() === 'audit'"
            (click)="active.set('audit')"
          >
            Audit
          </button>
        }
        @if (canProvision()) {
          <button
            type="button"
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="active() === 'provisioning'"
            [attr.aria-selected]="active() === 'provisioning'"
            (click)="openProvisioning()"
          >
            Provisioning
          </button>
        }
      </nav>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (active() === 'variables') {
        <akd-card title="Shared variables" [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Shared variables of this team
            </caption>
            <thead>
              <tr>
                <th scope="col">Key</th>
                <th scope="col">Scope</th>
                <th scope="col">Value</th>
                <th scope="col">Secret</th>
                <th scope="col" class="right"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (variable of variables(); track variable.uuid) {
                <tr>
                  <td>
                    <span class="akd-mono">{{ variable.key }}</span>
                    <div class="ref akd-mono akd-muted">{{ reference(variable) }}</div>
                  </td>
                  <td><span class="akd-badge akd-badge--mono">{{ variable.scope }}</span></td>
                  <td class="akd-mono akd-muted">
                    {{ variable.is_redacted ? '••••••••' : (variable.value ?? '—') }}
                  </td>
                  <td>
                    @if (variable.is_secret) {
                      <span class="akd-badge akd-badge--accent">read:sensitive</span>
                    } @else {
                      <span class="akd-badge">plain</span>
                    }
                  </td>
                  <td class="right">
                    <button
                      class="akd-btn akd-btn--danger akd-btn--sm"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(variable)"
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              }
              <!-- The last row IS the creator: a team-scoped variable in place. -->
              <tr class="add-row">
                <td>
                  <input
                    class="akd-input akd-input--mono"
                    name="newKey"
                    placeholder="NEW_KEY"
                    aria-label="New variable key"
                    [(ngModel)]="key"
                    [disabled]="busy()"
                    (keydown.enter)="create()"
                  />
                </td>
                <td><span class="akd-badge akd-badge--mono">team</span></td>
                <td>
                  <input
                    class="akd-input akd-input--mono"
                    name="newValue"
                    placeholder="value"
                    aria-label="New variable value"
                    [(ngModel)]="value"
                    [disabled]="busy()"
                    (keydown.enter)="create()"
                  />
                </td>
                <td>
                  <label class="akd-check" title="Value redacted without read:sensitive (INV-003)">
                    <input type="checkbox" name="newSecret" [(ngModel)]="secret" [disabled]="busy()" />
                    secret
                  </label>
                </td>
                <td class="right">
                  <button
                    class="akd-btn akd-btn--primary akd-btn--sm"
                    type="button"
                    [disabled]="busy() || !key.trim()"
                    (click)="create()"
                  >
                    <akd-icon name="plus" [size]="13" />
                    Add
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </akd-card>

        <p class="footnote">
          Reference these anywhere in a resource's env as
          <code class="akd-mono">{{ '{{' }}team.KEY{{ '}}' }}</code> — for example
          <code class="akd-mono">SENTRY_ORG={{ '{{' }}team.SENTRY_ORG{{ '}}' }}</code> (the scope prefix
          matches the Scope column). Interpolated at deploy time; an unknown reference stays verbatim
          in the container — visible, therefore diagnosable. Previews never receive shared secrets.
        </p>
      } @else if (active() === 'audit') {
        <akd-audit-log [fetch]="fetchAudit" exportName="team-audit" />
      } @else if (active() === 'provisioning') {
        <akd-card title="SCIM provisioning">
          <div class="stack">
            <p class="akd-muted sm">
              Provision and deprovision members automatically from your identity provider (Okta,
              Azure AD, Google). Configure the base URL below and a token as the bearer credential.
            </p>
            @if (createdToken(); as created) {
              <div class="secret">
                <p class="sm">
                  Token created — copied once. Configure your IdP with:
                </p>
                <div class="kv"><span class="k">SCIM base URL</span><code>{{ created.scim_base_url }}</code></div>
                <div class="kv"><span class="k">Token</span><code>{{ created.token }}</code></div>
              </div>
            }
            <form class="inline" (ngSubmit)="createScim()">
              <input
                class="akd-input"
                name="scimName"
                placeholder="Token name (e.g. okta-prod)"
                [(ngModel)]="scimName"
                [disabled]="busy()"
              />
              <button class="akd-btn akd-btn--primary akd-btn--sm" type="submit" [disabled]="busy() || !scimName.trim()">
                Create token
              </button>
            </form>

            @if (scimTokens().length === 0) {
              <p class="akd-muted sm">No SCIM tokens.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">SCIM tokens</caption>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Last used</th>
                    <th scope="col" class="right"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  @for (tok of scimTokens(); track tok.uuid) {
                    <tr>
                      <td>{{ tok.name }}</td>
                      <td class="akd-muted">{{ tok.last_used_at ? (tok.last_used_at | slice: 0 : 10) : 'never' }}</td>
                      <td class="right">
                        <button
                          class="akd-btn akd-btn--danger akd-btn--sm"
                          type="button"
                          [disabled]="busy()"
                          (click)="revokeScim(tok)"
                        >
                          Revoke
                        </button>
                      </td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          </div>
        </akd-card>
      } @else {
        <akd-card title="Team" class="cfg">
          <form class="cfgform" (ngSubmit)="saveConfig()">
            <div class="akd-field">
              <label class="akd-field__label" for="team-name">Name</label>
              <input
                id="team-name"
                name="name"
                class="akd-input"
                [(ngModel)]="cfgName"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="team-desc">Description</label>
              <textarea
                id="team-desc"
                name="description"
                class="akd-input"
                rows="3"
                [(ngModel)]="cfgDescription"
                [disabled]="busy()"
              ></textarea>
            </div>
            <div>
              <button
                class="akd-btn akd-btn--primary"
                type="submit"
                [disabled]="busy() || !cfgName.trim() || !cfgDirty()"
              >
                Save changes
              </button>
            </div>
          </form>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .ref {
        font-size: var(--text-xs);
        margin-top: 2px;
      }
      .add-row td {
        vertical-align: middle;
      }
      .add-row .akd-input {
        width: 100%;
      }
      .footnote {
        margin-top: var(--space-3);
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .footnote code {
        color: var(--text-2);
      }
      .cfg {
        display: block;
        max-width: 40rem;
      }
      .cfgform {
        display: grid;
        gap: var(--space-4);
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .sub-mono {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
      }
      .stack {
        display: grid;
        gap: var(--space-4);
      }
      .sm {
        font-size: var(--text-sm);
        margin: 0;
      }
      .inline {
        display: flex;
        gap: var(--space-2);
        align-items: center;
      }
      .inline .akd-input {
        max-width: 24rem;
      }
      .secret {
        display: grid;
        gap: var(--space-2);
        padding: var(--space-3);
        border: 1px dashed var(--accent-border);
        border-radius: var(--radius-2);
        background: var(--bg-inset);
      }
      .kv {
        display: flex;
        gap: var(--space-3);
        align-items: baseline;
      }
      .kv .k {
        min-width: 8rem;
        color: var(--text-3);
        font-size: var(--text-xs);
      }
      .kv code {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        word-break: break-all;
      }
    `,
  ],
})
export class TeamSettingsComponent {
  private readonly api = inject(ApiService);

  protected readonly variables = signal<SharedVariable[]>([]);
  protected readonly team = signal<Team | null>(null);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly active = signal<SettingsTab>('variables');

  // Audit tab (gated by audit:read). The reusable viewer loads itself when the
  // tab is first rendered; we just hand it a team-scoped fetcher.
  protected readonly canAudit = computed(() =>
    (this.api.currentUser()?.permissions ?? []).some((p) => p === 'audit:read' || p === 'root'),
  );
  protected readonly fetchAudit: AuditFetch = (query) => {
    const teamUuid = this.api.currentUser()?.teamUuid ?? '';
    return this.api.client().listTeamAudit(teamUuid, query);
  };

  // Provisioning tab (SCIM), gated by members:manage.
  protected readonly canProvision = computed(() =>
    (this.api.currentUser()?.permissions ?? []).some((p) => p === 'members:manage' || p === 'root'),
  );
  protected readonly scimTokens = signal<ScimToken[]>([]);
  protected readonly createdToken = signal<ScimTokenCreated | null>(null);
  private scimLoaded = false;
  protected scimName = '';

  protected key = '';
  protected value = '';
  protected secret = false;

  protected cfgName = '';
  protected cfgDescription = '';

  constructor() {
    void this.load();
  }

  protected openProvisioning(): void {
    this.active.set('provisioning');
    if (this.scimLoaded) return;
    this.scimLoaded = true;
    void this.loadScim();
  }

  private async loadScim(): Promise<void> {
    const teamUuid = this.api.currentUser()?.teamUuid;
    if (!teamUuid) return;
    try {
      const page = await this.api.client().listScimTokens(teamUuid);
      this.scimTokens.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async createScim(): Promise<void> {
    const teamUuid = this.api.currentUser()?.teamUuid;
    if (!teamUuid || !this.scimName.trim() || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const created = await this.api.client().createScimToken(teamUuid, { name: this.scimName.trim() });
      this.createdToken.set(created);
      this.scimName = '';
      await this.loadScim();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revokeScim(token: ScimToken): Promise<void> {
    const teamUuid = this.api.currentUser()?.teamUuid;
    if (!teamUuid || !confirm(`Revoke the SCIM token "${token.name}"? Provisioning with it stops immediately.`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().revokeScimToken(teamUuid, token.uuid);
      await this.loadScim();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected reference(variable: SharedVariable): string {
    const scope: Scope = variable.scope;
    return `{{${scope}.${variable.key}}}`;
  }

  protected cfgDirty(): boolean {
    const team = this.team();
    if (!team) return false;
    return this.cfgName.trim() !== team.name || this.cfgDescription !== (team.description ?? '');
  }

  private async load(): Promise<void> {
    try {
      const [variables, teams] = await Promise.all([
        this.api.client().listSharedVariables({ limit: 100 }),
        this.api.client().listTeams({ limit: 1 }),
      ]);
      this.variables.set(variables.data);
      const team = teams.data[0] ?? null;
      this.team.set(team);
      this.cfgName = team?.name ?? '';
      this.cfgDescription = team?.description ?? '';
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.key.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createSharedVariable({
        scope: 'team',
        key: this.key.trim(),
        value: this.value,
        is_secret: this.secret,
      });
      this.key = '';
      this.value = '';
      this.secret = false;
      await this.reloadVariables();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(variable: SharedVariable): Promise<void> {
    if (!confirm(`Delete the shared variable "${variable.key}"?`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteSharedVariable(variable.uuid);
      await this.reloadVariables();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async saveConfig(): Promise<void> {
    const team = this.team();
    if (!team || this.busy() || !this.cfgName.trim() || !this.cfgDirty()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const updated = await this.api.client().updateTeam(team.uuid, {
        name: this.cfgName.trim(),
        description: this.cfgDescription.trim() || null,
      });
      this.team.set(updated);
      this.cfgName = updated.name;
      this.cfgDescription = updated.description ?? '';
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async reloadVariables(): Promise<void> {
    const page = await this.api.client().listSharedVariables({ limit: 100 });
    this.variables.set(page.data);
  }
}
