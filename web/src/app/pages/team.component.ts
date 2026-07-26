import { SlicePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ModalComponent } from '../../ui/modal/modal.component';
import type { components } from '../../api/schema';

type Team = components['schemas']['Team'];
type TeamMember = components['schemas']['TeamMember'];
type Invitation = components['schemas']['Invitation'];

type TeamTab = 'members' | 'pending' | 'canceled';

/**
 * Team members page (design kit: MembersScreen). Members, still-open
 * invitations and closed ones (revoked/expired) each get their own tab.
 * Invitations are created in a modal; the one-time invite link stays in the
 * modal until it is closed — only its hash survives server-side. API tokens
 * live in Personal settings (they are minted per operator, not per team page).
 */
@Component({
  selector: 'app-team',
  standalone: true,
  imports: [FormsModule, SlicePipe, CardComponent, IconComponent, ModalComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  // Any click outside an open row menu dismisses it (the toggle stops
  // propagation so opening one does not immediately close it).
  host: { '(document:click)': 'closeMenus()' },
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
      @if (notice(); as message) {
        <p class="akd-muted" role="status">{{ message }}</p>
      }

      @if (team()?.description; as description) {
        <p class="akd-muted desc">{{ description }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else {
        <nav class="akd-tabs" role="tablist" aria-label="Team sections">
          <button
            type="button"
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="tab() === 'members'"
            [attr.aria-selected]="tab() === 'members'"
            (click)="tab.set('members')"
          >
            Members
            <span class="akd-tab__count">{{ members().length }}</span>
          </button>
          <button
            type="button"
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="tab() === 'pending'"
            [attr.aria-selected]="tab() === 'pending'"
            (click)="tab.set('pending')"
          >
            Pending invitations
            @if (pending().length > 0) {
              <span class="akd-tab__count">{{ pending().length }}</span>
            }
          </button>
          <button
            type="button"
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="tab() === 'canceled'"
            [attr.aria-selected]="tab() === 'canceled'"
            (click)="tab.set('canceled')"
          >
            Canceled
            @if (canceled().length > 0) {
              <span class="akd-tab__count">{{ canceled().length }}</span>
            }
          </button>
        </nav>

        @switch (tab()) {
          @case ('members') {
            <akd-card title="Members" [padded]="false">
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
                            [class.akd-badge--accent]="member.role === 'admin'"
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
          }

          @case ('pending') {
            <akd-card title="Pending invitations" [padded]="false">
              @if (pending().length === 0) {
                <p class="akd-muted pad">No pending invitations.</p>
              } @else {
                <table class="akd-table">
                  <caption class="sr-only">
                    Invitations still open
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Email</th>
                      <th scope="col">Role</th>
                      <th scope="col">Expires</th>
                      <th scope="col"><span class="sr-only">Actions</span></th>
                    </tr>
                  </thead>
                  <tbody>
                    @for (inv of pending(); track inv.uuid) {
                      <tr>
                        <td class="akd-mono">{{ inv.email }}</td>
                        <td>
                          <span class="akd-badge akd-badge--mono">{{ inv.role }}</span>
                        </td>
                        <td class="akd-muted">{{ inv.expires_at | slice: 0 : 10 }}</td>
                        <td class="right">
                          <div class="menu">
                            <button
                              class="akd-iconbtn"
                              type="button"
                              [disabled]="busy()"
                              [attr.aria-expanded]="menuFor() === inv.uuid"
                              aria-label="Invitation actions"
                              (click)="toggleMenu(inv.uuid, $event)"
                            >
                              <akd-icon name="more-horizontal" [size]="15" />
                            </button>
                            @if (menuFor() === inv.uuid) {
                              <div class="menu__list" role="menu">
                                <button
                                  class="menu__item"
                                  type="button"
                                  role="menuitem"
                                  (click)="copyInviteLink(inv)"
                                >
                                  <akd-icon name="copy" [size]="14" />
                                  Copy invitation link
                                </button>
                                <button
                                  class="menu__item menu__item--danger"
                                  type="button"
                                  role="menuitem"
                                  (click)="revokeInvitation(inv)"
                                >
                                  <akd-icon name="x" [size]="14" />
                                  Cancel invitation
                                </button>
                              </div>
                            }
                          </div>
                        </td>
                      </tr>
                    }
                  </tbody>
                </table>
              }
            </akd-card>
          }

          @case ('canceled') {
            <akd-card title="Canceled invitations" [padded]="false">
              @if (canceled().length === 0) {
                <p class="akd-muted pad">No canceled or expired invitations.</p>
              } @else {
                <table class="akd-table">
                  <caption class="sr-only">
                    Revoked or expired invitations
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Email</th>
                      <th scope="col">Role</th>
                      <th scope="col">Status</th>
                      <th scope="col">Expires</th>
                    </tr>
                  </thead>
                  <tbody>
                    @for (inv of canceled(); track inv.uuid) {
                      <tr>
                        <td class="akd-mono">{{ inv.email }}</td>
                        <td>
                          <span class="akd-badge akd-badge--mono">{{ inv.role }}</span>
                        </td>
                        <td>
                          <span class="akd-badge akd-badge--mono">{{ inv.status }}</span>
                        </td>
                        <td class="akd-muted">{{ inv.expires_at | slice: 0 : 10 }}</td>
                      </tr>
                    }
                  </tbody>
                </table>
              }
            </akd-card>
          }
        }

        <p class="footnote">
          Invitation links are shown once — only their SHA-256 is stored. Email delivery is an
          addition: the link stays in the response even without a configured relay.
        </p>
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
                  <option value="admin">admin</option>
                  <option value="member">member</option>
                  <option value="reviewer">reviewer</option>
                </select>
              </div>
              <span class="akd-field__hint">
                Admin: full control of the team. Member: manages resources.
                Reviewer: sees PR previews only. Instance settings stay
                administrator-only.
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
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .footnote {
        margin: var(--space-5) 0 0;
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
      .menu {
        position: relative;
        display: inline-block;
      }
      .menu__list {
        position: absolute;
        top: 100%;
        right: 0;
        margin-top: 4px;
        min-width: 200px;
        background: var(--bg-3);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-3);
        box-shadow: var(--shadow-2);
        padding: 4px;
        z-index: 50;
        display: grid;
        gap: 2px;
        animation: akd-slide-in var(--dur-1) var(--ease-out);
      }
      .menu__item {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        width: 100%;
        padding: var(--space-2) var(--space-3);
        border: 0;
        border-radius: var(--radius-2);
        background: transparent;
        color: var(--text-1);
        font: inherit;
        text-align: left;
        cursor: pointer;
      }
      .menu__item:hover {
        background: var(--bg-2);
      }
      .menu__item:focus-visible {
        outline: none;
        box-shadow: var(--ring-focus);
      }
      .menu__item--danger {
        color: var(--danger, var(--text-1));
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
    `,
  ],
})
export class TeamComponent {
  private readonly api = inject(ApiService);

  protected readonly tab = signal<TeamTab>('members');
  protected readonly team = signal<Team | null>(null);
  protected readonly members = signal<TeamMember[]>([]);
  protected readonly invitations = signal<Invitation[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly inviteOpen = signal(false);
  protected readonly copied = signal(false);
  /** UUID of the invitation whose row menu is open, or null. */
  protected readonly menuFor = signal<string | null>(null);
  /** Transient status line (link copied / email re-sent). */
  protected readonly notice = signal<string | null>(null);
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;
  /** One-time invitation link — the server never returns it again. */
  protected readonly inviteLink = signal<string | null>(null);
  protected readonly inviteSent = signal<string | null>(null);

  /** Still open — the only invitations that can be acted on. */
  protected readonly pending = computed(() =>
    this.invitations().filter((inv) => inv.status === 'pending'),
  );
  /** Closed for good: revoked by an admin or timed out. Accepted ones are not
   * shown — the person is in Members. */
  protected readonly canceled = computed(() =>
    this.invitations().filter((inv) => inv.status === 'revoked' || inv.status === 'expired'),
  );

  protected inviteEmail = '';
  protected inviteRole: 'admin' | 'member' | 'reviewer' = 'member';

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

  protected toggleMenu(uuid: string, event: Event): void {
    // Stop the document listener from closing what this click just opened.
    event.stopPropagation();
    this.menuFor.set(this.menuFor() === uuid ? null : uuid);
  }

  protected closeMenus(): void {
    this.menuFor.set(null);
  }

  private flashNotice(text: string): void {
    this.notice.set(text);
    if (this.noticeTimer !== null) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => this.notice.set(null), 4000);
  }

  /** Regenerate the invitation's link and copy the fresh one — the previous
   * link is invalidated, and the email is re-sent when a relay is configured. */
  protected async copyInviteLink(inv: Invitation): Promise<void> {
    this.menuFor.set(null);
    if (!this.teamUuid) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const refreshed = await this.api.client().resendTeamInvitation(this.teamUuid, inv.uuid);
      if (refreshed.invite_url) {
        await navigator.clipboard.writeText(refreshed.invite_url);
        this.flashNotice(
          `New invitation link for ${inv.email} copied — the previous link no longer works.`,
        );
      } else {
        this.flashNotice(`Invitation email re-sent to ${inv.email}.`);
      }
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openInvite(): void {
    this.inviteLink.set(null);
    this.inviteSent.set(null);
    this.copied.set(false);
    this.inviteOpen.set(true);
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
      const [team, members, invitations] = await Promise.all([
        client.getTeam(teamUuid),
        client.listTeamMembers(teamUuid, { limit: 100 }),
        client.listTeamInvitations(teamUuid, { limit: 100 }),
      ]);
      this.team.set(team);
      this.members.set(members.data);
      this.invitations.set(invitations.data);
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
    this.menuFor.set(null);
    if (!this.teamUuid) return;
    if (!confirm(`Cancel the invitation for ${inv.email}? Its link stops working.`)) return;
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
}
