import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Team = components['schemas']['Team'];
type TeamMember = components['schemas']['TeamMember'];
type Invitation = components['schemas']['Invitation'];
type ApiToken = components['schemas']['ApiToken'];
type ApiTokenPermission = components['schemas']['ApiTokenPermission'];

type Tab = 'members' | 'invitations' | 'tokens';

const PERMISSIONS: ApiTokenPermission[] = ['read', 'read:sensitive', 'write', 'deploy', 'root'];

@Component({
  selector: 'app-team',
  standalone: true,
  imports: [FormsModule, SlicePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Team</h1>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (team(); as t) {
        <dl class="akd-dl head">
          <dt>Name</dt>
          <dd>{{ t.name }}</dd>
          @if (t.description) {
            <dt>Description</dt>
            <dd>{{ t.description }}</dd>
          }
        </dl>
      }

      <nav class="akd-tabs" role="tablist" aria-label="Team sections">
        <button type="button" role="tab" [attr.aria-selected]="tab() === 'members'" (click)="tab.set('members')">
          Members
        </button>
        <button type="button" role="tab" [attr.aria-selected]="tab() === 'invitations'" (click)="tab.set('invitations')">
          Invitations
        </button>
        <button type="button" role="tab" [attr.aria-selected]="tab() === 'tokens'" (click)="tab.set('tokens')">
          API tokens
        </button>
      </nav>

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else {
        @switch (tab()) {
          @case ('members') {
            @if (members().length === 0) {
              <div class="akd-empty"><p><strong>No members.</strong></p></div>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">Members of this team</caption>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Email</th>
                    <th scope="col">Role</th>
                    <th scope="col">Joined</th>
                  </tr>
                </thead>
                <tbody>
                  @for (member of members(); track member.user_uuid) {
                    <tr>
                      <td>{{ member.name ?? '—' }}</td>
                      <td class="akd-muted">{{ member.email }}</td>
                      <td class="akd-muted">{{ member.role }}</td>
                      <td class="akd-muted">{{ member.joined_at | slice: 0 : 10 }}</td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          }

          @case ('invitations') {
            <section class="akd-card">
              <h2>Invite someone</h2>
              <form class="row" (ngSubmit)="invite()">
                <div class="akd-field grow">
                  <label for="inv-email">Email</label>
                  <input
                    id="inv-email"
                    name="email"
                    type="email"
                    class="akd-input"
                    [(ngModel)]="inviteEmail"
                    [disabled]="busy()"
                    required
                  />
                </div>
                <div class="akd-field">
                  <label for="inv-role">Role</label>
                  <select id="inv-role" name="role" class="akd-select" [(ngModel)]="inviteRole">
                    <option value="member">member</option>
                    <option value="admin">admin</option>
                  </select>
                </div>
                <button class="akd-btn" type="submit" [disabled]="busy()">Invite</button>
              </form>

              @if (inviteLink(); as link) {
                <div>
                  <p class="akd-muted hint">
                    One-time invitation link — shown once, copy it now and pass it to the invitee.
                  </p>
                  <p class="akd-secret">{{ link }}</p>
                </div>
              } @else if (inviteSent(); as email) {
                <p class="akd-muted" role="status">Invitation email sent to {{ email }}.</p>
              }
            </section>

            @if (invitations().length === 0) {
              <div class="akd-empty"><p><strong>No invitations.</strong></p></div>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">Pending and past invitations</caption>
                <thead>
                  <tr>
                    <th scope="col">Email</th>
                    <th scope="col">Role</th>
                    <th scope="col">Status</th>
                    <th scope="col">Expires</th>
                    <th scope="col"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  @for (inv of invitations(); track inv.uuid) {
                    <tr>
                      <td>{{ inv.email }}</td>
                      <td class="akd-muted">{{ inv.role }}</td>
                      <td class="akd-muted">{{ inv.status }}</td>
                      <td class="akd-muted">{{ inv.expires_at | slice: 0 : 10 }}</td>
                      <td class="right">
                        @if (inv.status === 'pending') {
                          <button
                            class="akd-btn-danger"
                            type="button"
                            [disabled]="busy()"
                            (click)="revokeInvitation(inv)"
                          >
                            Revoke
                          </button>
                        }
                      </td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          }

          @case ('tokens') {
            <section class="akd-card">
              <h2>Create a token</h2>
              <form class="form" (ngSubmit)="createToken()">
                <div class="akd-field">
                  <label for="tok-name">Name</label>
                  <input
                    id="tok-name"
                    name="name"
                    class="akd-input"
                    placeholder="e.g. ci-github-actions"
                    [(ngModel)]="tokenName"
                    [disabled]="busy()"
                    required
                  />
                </div>
                <fieldset class="perms">
                  <legend>Permissions</legend>
                  @for (perm of permissions; track perm) {
                    <label class="check">
                      <input
                        type="checkbox"
                        [name]="'perm-' + perm"
                        [(ngModel)]="tokenPerms[perm]"
                      />
                      {{ perm }}
                    </label>
                  }
                </fieldset>
                <div>
                  <button class="akd-btn" type="submit" [disabled]="busy()">Create token</button>
                </div>
              </form>

              @if (tokenValue(); as value) {
                <div>
                  <p class="akd-muted hint">
                    Token value — shown once, copy it now. Only its hash is stored.
                  </p>
                  <p class="akd-secret">{{ value }}</p>
                </div>
              }
            </section>

            @if (tokens().length === 0) {
              <div class="akd-empty"><p><strong>No API tokens.</strong></p></div>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">API tokens of this team</caption>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Prefix</th>
                    <th scope="col">Permissions</th>
                    <th scope="col">Last used</th>
                    <th scope="col"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  @for (token of tokens(); track token.uuid) {
                    <tr>
                      <td>{{ token.name }}</td>
                      <td class="akd-mono">{{ token.token_prefix }}…</td>
                      <td class="akd-muted">{{ token.permissions.join(', ') }}</td>
                      <td class="akd-muted">
                        {{ token.last_used_at ? (token.last_used_at | slice: 0 : 10) : 'never' }}
                      </td>
                      <td class="right">
                        <button
                          class="akd-btn-danger"
                          type="button"
                          [disabled]="busy()"
                          (click)="revokeToken(token)"
                        >
                          Revoke
                        </button>
                      </td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          }
        }
      }
    </div>
  `,
  styles: [
    `
      .head {
        margin-bottom: var(--akd-space-5);
      }
      .akd-card {
        margin-bottom: var(--akd-space-5);
      }
      .form {
        display: grid;
        gap: var(--akd-space-3);
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--akd-space-3);
        flex-wrap: wrap;
      }
      .grow {
        flex: 1;
        min-width: 220px;
      }
      .perms {
        display: flex;
        gap: var(--akd-space-4);
        flex-wrap: wrap;
        margin: 0;
        padding: 0;
        border: 0;
      }
      .perms legend {
        font-size: var(--akd-text-sm);
        font-weight: var(--akd-weight-medium);
        padding: 0;
        margin-bottom: var(--akd-space-1);
      }
      .check {
        display: flex;
        align-items: center;
        gap: var(--akd-space-1);
        font-size: var(--akd-text-sm);
      }
      .hint {
        margin: 0 0 var(--akd-space-1);
        font-size: var(--akd-text-xs);
      }
      p.akd-secret {
        margin: 0;
      }
    `,
  ],
})
export class TeamComponent {
  private readonly api = inject(ApiService);

  protected readonly permissions = PERMISSIONS;
  protected readonly tab = signal<Tab>('members');
  protected readonly team = signal<Team | null>(null);
  protected readonly members = signal<TeamMember[]>([]);
  protected readonly invitations = signal<Invitation[]>([]);
  protected readonly tokens = signal<ApiToken[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  /** One-time invitation link — the server never returns it again. */
  protected readonly inviteLink = signal<string | null>(null);
  protected readonly inviteSent = signal<string | null>(null);
  /** One-time token value — only its hash survives on the server. */
  protected readonly tokenValue = signal<string | null>(null);

  protected inviteEmail = '';
  protected inviteRole: 'member' | 'admin' = 'member';
  protected tokenName = '';
  protected tokenPerms: Record<ApiTokenPermission, boolean> = {
    read: true,
    'read:sensitive': false,
    write: false,
    deploy: false,
    root: false,
  };

  private readonly teamUuid = this.api.currentUser()?.teamUuid ?? null;

  constructor() {
    if (!this.teamUuid) {
      this.error.set('No team in the current session — sign in again.');
      this.loading.set(false);
    } else {
      void this.load(this.teamUuid);
    }
  }

  private async load(teamUuid: string): Promise<void> {
    try {
      const client = this.api.client();
      const [team, members, invitations, tokens] = await Promise.all([
        client.getTeam(teamUuid),
        client.listTeamMembers(teamUuid, { limit: 100 }),
        client.listTeamInvitations(teamUuid, { limit: 100 }),
        client.listApiTokens(teamUuid, { limit: 100 }),
      ]);
      this.team.set(team);
      this.members.set(members.data);
      this.invitations.set(invitations.data);
      this.tokens.set(tokens.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async invite(): Promise<void> {
    if (!this.teamUuid || !this.inviteEmail.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    this.inviteLink.set(null);
    this.inviteSent.set(null);
    try {
      const created = await this.api.client().createTeamInvitation(this.teamUuid, {
        email: this.inviteEmail.trim(),
        role: this.inviteRole,
        expires_in_hours: 168,
      });
      // The link is only present when the instance has no transactional email
      // configured (manual hand-off) — and then only in this response, once.
      if (created.invite_url) {
        this.inviteLink.set(created.invite_url);
      } else {
        this.inviteSent.set(created.email);
      }
      this.inviteEmail = '';
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revokeInvitation(inv: Invitation): Promise<void> {
    if (!this.teamUuid) return;
    if (!confirm(`Revoke the invitation for ${inv.email}? Its link stops working.`)) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().revokeTeamInvitation(this.teamUuid, inv.uuid);
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async createToken(): Promise<void> {
    if (!this.teamUuid || !this.tokenName.trim()) return;
    const permissions = PERMISSIONS.filter((p) => this.tokenPerms[p]);
    if (permissions.length === 0) {
      this.error.set('Select at least one permission.');
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    this.tokenValue.set(null);
    try {
      const created = await this.api.client().createApiToken(this.teamUuid, {
        name: this.tokenName.trim(),
        permissions,
        ip_allowlist: [],
      });
      this.tokenValue.set(created.token);
      this.tokenName = '';
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revokeToken(token: ApiToken): Promise<void> {
    if (!this.teamUuid) return;
    if (
      !confirm(
        `Revoke the token "${token.name}"? Every script or CI job using it stops working immediately.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().revokeApiToken(this.teamUuid, token.uuid);
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
