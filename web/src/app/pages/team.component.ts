import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ModalComponent } from '../../ui/modal/modal.component';
import type { components } from '../../api/schema';

type Team = components['schemas']['Team'];
type TeamMember = components['schemas']['TeamMember'];
type Invitation = components['schemas']['Invitation'];
type ApiToken = components['schemas']['ApiToken'];
type ApiTokenPermission = components['schemas']['ApiTokenPermission'];

const PERMISSIONS: ApiTokenPermission[] = ['read', 'read:sensitive', 'write', 'deploy', 'root'];

/**
 * Team members page (design kit: MembersScreen). Invitations and API tokens
 * are created in modals; their one-time secrets (invite link, token value)
 * stay in the modal until it is closed — only their hash survives server-side.
 */
@Component({
  selector: 'app-team',
  standalone: true,
  imports: [FormsModule, SlicePipe, CardComponent, IconComponent, ModalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Members</h1>
        @if (team(); as t) {
          <span class="akd-badge akd-badge--mono">team {{ t.name }}</span>
        }
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="openInvite()">
          <akd-icon name="user-plus" [size]="15" />
          Invite member
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (team()?.description; as description) {
        <p class="akd-muted desc">{{ description }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else {
        <div class="stack">
          <akd-card title="Members" [padded]="false">
            <span card-actions class="akd-badge akd-badge--mono">
              {{ members().length }} members
            </span>
            @if (members().length === 0) {
              <p class="akd-muted pad">No members.</p>
            } @else {
              <table class="akd-table">
                <caption class="sr-only">
                  Members of this team
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Member</th>
                    <th scope="col">Role</th>
                    <th scope="col">Joined</th>
                  </tr>
                </thead>
                <tbody>
                  @for (member of members(); track member.user_uuid) {
                    <tr>
                      <td>
                        <span class="member-cell">
                          <span class="avatar" aria-hidden="true">{{ initials(member) }}</span>
                          <span class="member-id">
                            <span class="member-name">{{ member.name ?? member.email }}</span>
                            @if (member.name) {
                              <span class="sub-mono">{{ member.email }}</span>
                            }
                          </span>
                        </span>
                      </td>
                      <td>
                        <span
                          class="akd-badge akd-badge--mono"
                          [class.akd-badge--accent]="member.role === 'owner'"
                        >
                          {{ member.role }}
                        </span>
                      </td>
                      <td class="akd-muted">{{ member.joined_at | slice: 0 : 10 }}</td>
                    </tr>
                  }
                </tbody>
              </table>
            }
          </akd-card>

          <div class="grid2">
            <akd-card title="Pending invitations" [padded]="false">
              @if (invitations().length === 0) {
                <p class="akd-muted pad">No invitations.</p>
              } @else {
                <table class="akd-table">
                  <caption class="sr-only">
                    Pending and past invitations
                  </caption>
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
                        <td class="akd-mono">{{ inv.email }}</td>
                        <td>
                          <span class="akd-badge akd-badge--mono">{{ inv.role }}</span>
                        </td>
                        <td>
                          <span class="akd-badge akd-badge--mono">{{ inv.status }}</span>
                        </td>
                        <td class="akd-muted">{{ inv.expires_at | slice: 0 : 10 }}</td>
                        <td class="right">
                          @if (inv.status === 'pending') {
                            <button
                              class="akd-iconbtn"
                              type="button"
                              [disabled]="busy()"
                              (click)="revokeInvitation(inv)"
                              aria-label="Revoke invitation"
                            >
                              <akd-icon name="x" [size]="15" />
                            </button>
                          }
                        </td>
                      </tr>
                    }
                  </tbody>
                </table>
              }
            </akd-card>

            <akd-card title="API tokens" [padded]="false">
              <button
                card-actions
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                (click)="openToken()"
              >
                <akd-icon name="plus" [size]="13" />
                New token
              </button>
              @if (tokens().length === 0) {
                <p class="akd-muted pad">No API tokens.</p>
              } @else {
                <table class="akd-table">
                  <caption class="sr-only">
                    API tokens of this team
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Token</th>
                      <th scope="col">Permissions</th>
                      <th scope="col">Last used</th>
                      <th scope="col"><span class="sr-only">Actions</span></th>
                    </tr>
                  </thead>
                  <tbody>
                    @for (token of tokens(); track token.uuid) {
                      <tr>
                        <td>
                          <span class="member-id">
                            <span class="member-name akd-mono">{{ token.name }}</span>
                            <span class="sub-mono">{{ token.token_prefix }}…</span>
                          </span>
                        </td>
                        <td>
                          <span
                            class="akd-badge akd-badge--mono"
                            [class.akd-badge--danger]="token.permissions.includes('root')"
                          >
                            {{ token.permissions.join(' · ') }}
                          </span>
                        </td>
                        <td class="akd-muted">
                          {{ token.last_used_at ? (token.last_used_at | slice: 0 : 10) : 'never' }}
                        </td>
                        <td class="right">
                          <button
                            class="akd-iconbtn"
                            type="button"
                            [disabled]="busy()"
                            (click)="revokeToken(token)"
                            aria-label="Revoke token"
                          >
                            <akd-icon name="trash-2" [size]="15" />
                          </button>
                        </td>
                      </tr>
                    }
                  </tbody>
                </table>
              }
            </akd-card>
          </div>

          <p class="footnote">
            Invitation links and token secrets are shown once — only their SHA-256 is stored. Email
            delivery is an addition: the link stays in the response even without a configured relay.
          </p>
        </div>
      }

      <akd-modal [open]="inviteOpen()" title="Invite a member" (closed)="inviteOpen.set(false)">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (inviteLink(); as link) {
          <div class="modal-stack">
            <span>
              Invitation created. The link below is shown <strong>once</strong> — only its hash is
              stored.
            </span>
            <div class="secret-line">
              <code>{{ link }}</code>
              <button
                class="akd-iconbtn akd-iconbtn--bordered"
                type="button"
                (click)="copy(link)"
                aria-label="Copy link"
              >
                <akd-icon [name]="copied() ? 'check' : 'copy'" [size]="15" />
              </button>
            </div>
          </div>
        } @else if (inviteSent(); as email) {
          <p class="modal-status" role="status">Invitation email sent to {{ email }}.</p>
        } @else {
          <form id="invite-form" class="modal-stack" (ngSubmit)="invite()">
            <div class="akd-field">
              <label class="akd-field__label" for="inv-email">Email</label>
              <input
                id="inv-email"
                name="email"
                type="email"
                class="akd-input akd-input--mono"
                [(ngModel)]="inviteEmail"
                [disabled]="busy()"
                required
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="inv-role">Role</label>
              <div class="akd-select">
                <select
                  id="inv-role"
                  name="role"
                  class="akd-input"
                  [(ngModel)]="inviteRole"
                  [disabled]="busy()"
                >
                  <option value="member">member</option>
                  <option value="admin">admin</option>
                </select>
              </div>
              <span class="akd-field__hint">
                Admins can manage servers and destructive actions; root stays API-token only.
              </span>
            </div>
          </form>
        }
        <div modal-footer>
          @if (inviteLink() || inviteSent()) {
            <button class="akd-btn akd-btn--ghost" type="button" (click)="inviteOpen.set(false)">
              Close
            </button>
          } @else {
            <button
              class="akd-btn akd-btn--ghost"
              type="button"
              (click)="inviteOpen.set(false)"
              [disabled]="busy()"
            >
              Cancel
            </button>
            <button
              class="akd-btn akd-btn--primary"
              type="submit"
              form="invite-form"
              [disabled]="busy() || !inviteEmail.trim()"
            >
              <akd-icon name="send" [size]="15" />
              {{ busy() ? 'Sending…' : 'Send invitation' }}
            </button>
          }
        </div>
      </akd-modal>

      <akd-modal [open]="tokenOpen()" title="Create an API token" (closed)="tokenOpen.set(false)">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (tokenValue(); as value) {
          <div class="modal-stack">
            <span>
              Token created. The value below is shown <strong>once</strong> — only its hash is
              stored.
            </span>
            <div class="secret-line">
              <code>{{ value }}</code>
              <button
                class="akd-iconbtn akd-iconbtn--bordered"
                type="button"
                (click)="copy(value)"
                aria-label="Copy token"
              >
                <akd-icon [name]="copied() ? 'check' : 'copy'" [size]="15" />
              </button>
            </div>
          </div>
        } @else {
          <form id="token-form" class="modal-stack" (ngSubmit)="createToken()">
            <div class="akd-field">
              <label class="akd-field__label" for="tok-name">Name</label>
              <input
                id="tok-name"
                name="name"
                class="akd-input akd-input--mono"
                placeholder="e.g. ci-github-actions"
                [(ngModel)]="tokenName"
                [disabled]="busy()"
                required
              />
            </div>
            <fieldset class="perms">
              <legend class="akd-field__label">Permissions</legend>
              @for (perm of permissions; track perm) {
                <label class="akd-check">
                  <input
                    type="checkbox"
                    [name]="'perm-' + perm"
                    [(ngModel)]="tokenPerms[perm]"
                    [disabled]="busy()"
                  />
                  <span class="akd-mono">{{ perm }}</span>
                </label>
              }
            </fieldset>
          </form>
        }
        <div modal-footer>
          @if (tokenValue()) {
            <button class="akd-btn akd-btn--ghost" type="button" (click)="tokenOpen.set(false)">
              Close
            </button>
          } @else {
            <button
              class="akd-btn akd-btn--ghost"
              type="button"
              (click)="tokenOpen.set(false)"
              [disabled]="busy()"
            >
              Cancel
            </button>
            <button
              class="akd-btn akd-btn--primary"
              type="submit"
              form="token-form"
              [disabled]="busy() || !tokenName.trim()"
            >
              <akd-icon name="key" [size]="15" />
              {{ busy() ? 'Creating…' : 'Create token' }}
            </button>
          }
        </div>
      </akd-modal>
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .desc {
        margin: 0 0 var(--space-4);
        font-size: var(--text-sm);
      }
      .stack {
        display: grid;
        gap: var(--space-5);
      }
      .grid2 {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: var(--space-5);
        align-items: start;
      }
      @media (max-width: 960px) {
        .grid2 {
          grid-template-columns: 1fr;
        }
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .footnote {
        margin: 0;
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .member-cell {
        display: inline-flex;
        align-items: center;
        gap: var(--space-2);
      }
      .avatar {
        width: 26px;
        height: 26px;
        border-radius: 50%;
        background: var(--accent-dim);
        border: 1px solid var(--accent-border);
        display: grid;
        place-content: center;
        font: var(--weight-semibold) var(--text-2xs) var(--font-display);
        color: var(--accent);
        flex: none;
      }
      .member-id {
        display: grid;
      }
      .member-name {
        font-weight: var(--weight-medium);
      }
      .sub-mono {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      .modal-stack {
        display: grid;
        gap: var(--space-4);
      }
      .modal-status {
        margin: 0;
      }
      .secret-line {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        background: var(--bg-inset);
        border: 1px dashed var(--accent-border);
        border-radius: var(--radius-2);
        padding: var(--space-2) var(--space-3);
      }
      .secret-line code {
        flex: 1;
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        color: var(--text-1);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .perms {
        display: grid;
        gap: var(--space-2);
        margin: 0;
        padding: 0;
        border: 0;
      }
      .perms legend {
        padding: 0;
        margin-bottom: var(--space-1);
      }
    `,
  ],
})
export class TeamComponent {
  private readonly api = inject(ApiService);

  protected readonly permissions = PERMISSIONS;
  protected readonly team = signal<Team | null>(null);
  protected readonly members = signal<TeamMember[]>([]);
  protected readonly invitations = signal<Invitation[]>([]);
  protected readonly tokens = signal<ApiToken[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly inviteOpen = signal(false);
  protected readonly tokenOpen = signal(false);
  protected readonly copied = signal(false);
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

  protected initials(member: TeamMember): string {
    const source = (member.name ?? member.email).trim();
    return (
      source
        .split(/\s+/)
        .filter(Boolean)
        .slice(0, 2)
        .map((word) => word[0])
        .join('')
        .toUpperCase() || '?'
    );
  }

  protected openInvite(): void {
    this.inviteLink.set(null);
    this.inviteSent.set(null);
    this.copied.set(false);
    this.inviteOpen.set(true);
  }

  protected openToken(): void {
    this.tokenValue.set(null);
    this.copied.set(false);
    this.tokenOpen.set(true);
  }

  protected async copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      this.copied.set(true);
      setTimeout(() => this.copied.set(false), 2000);
    } catch {
      // Clipboard may be unavailable — the secret stays selectable in the box.
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
