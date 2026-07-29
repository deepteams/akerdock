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
type MemberAccessEntry = components['schemas']['MemberAccessEntry'];
type Invitation = components['schemas']['Invitation'];
type CustomRole = components['schemas']['CustomRole'];
type PermissionEntry = components['schemas']['PermissionCatalogEntry'];

type TeamTab = 'members' | 'roles' | 'pending' | 'canceled';

/** A group of permissions sharing a domain (the part before the colon), for the
 * custom-role composer. */
interface PermissionGroup {
  domain: string;
  permissions: PermissionEntry[];
}

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
            [class.akd-tab--active]="tab() === 'roles'"
            [attr.aria-selected]="tab() === 'roles'"
            (click)="tab.set('roles')"
          >
            Roles
            @if (customRoles().length > 0) {
              <span class="akd-tab__count">{{ customRoles().length }}</span>
            }
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
                      <th scope="col"><span class="sr-only">Actions</span></th>
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
                            {{ member.role === 'custom' ? member.custom_role_name : member.role }}
                          </span>
                        </td>
                        <td class="akd-muted">{{ member.joined_at | slice: 0 : 10 }}</td>
                        <td class="right">
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            (click)="toggleAccess(member)"
                          >
                            {{ accessFor() === member.user_uuid ? 'Hide access' : 'Access' }}
                          </button>
                          <button
                            class="akd-btn akd-btn--ghost akd-btn--sm"
                            type="button"
                            [disabled]="busy()"
                            (click)="openMemberRole(member)"
                          >
                            Change role
                          </button>
                        </td>
                      </tr>
                      @if (accessFor() === member.user_uuid) {
                        <tr>
                          <td colspan="4" class="access-row">
                            @if (access().length === 0) {
                              <span class="akd-muted">Loading…</span>
                            } @else {
                              <!-- What this member reaches, and through which role. One
                                   row: the team is the scope of a role (ADR-047). -->
                              @for (scope of access(); track scope.scope) {
                                <div class="access-scope">
                                  <span class="akd-badge akd-badge--mono">{{ scope.scope }}</span>
                                  <strong>{{ scope.role }}</strong>
                                  @if (scope.resource_count !== undefined) {
                                    <span class="akd-muted"
                                      >{{ scope.resource_count }} resources</span
                                    >
                                  }
                                  <span class="caps">
                                    @for (cap of scope.capabilities; track cap; let last = $last) {
                                      {{ cap }}
                                      @if (!last) {
                                        <span class="sep" aria-hidden="true">·</span>
                                      }
                                    }
                                  </span>
                                </div>
                              }
                            }
                          </td>
                        </tr>
                      }
                    }
                  </tbody>
                </table>
              }
            </akd-card>
          }

          @case ('roles') {
            <akd-card [padded]="false">
              <div class="card-head">
                <span>Custom roles</span>
                <button
                  class="akd-btn akd-btn--primary akd-btn--sm"
                  type="button"
                  (click)="openRole(null)"
                >
                  <akd-icon name="plus" [size]="14" />
                  New role
                </button>
              </div>
              @if (customRoles().length === 0) {
                <p class="akd-muted pad">
                  No custom roles yet. A custom role is a named set of granular permissions you can
                  assign to members, on top of the built-in admin / member / reviewer roles.
                </p>
              } @else {
                <table class="akd-table">
                  <caption class="sr-only">
                    Custom roles of this team
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Role</th>
                      <th scope="col">Permissions</th>
                      <th scope="col">Members</th>
                      <th scope="col"><span class="sr-only">Actions</span></th>
                    </tr>
                  </thead>
                  <tbody>
                    @for (role of customRoles(); track role.uuid) {
                      <tr>
                        <td>
                          <span class="member-id">
                            <span class="member-name">{{ role.name }}</span>
                            @if (role.description) {
                              <span class="sub-mono">{{ role.description }}</span>
                            }
                          </span>
                        </td>
                        <td class="akd-muted">{{ role.permissions.length }}</td>
                        <td class="akd-muted">{{ role.member_count ?? 0 }}</td>
                        <td class="right">
                          <div class="menu">
                            <button
                              class="akd-iconbtn"
                              type="button"
                              [disabled]="busy()"
                              [attr.aria-expanded]="menuFor() === role.uuid"
                              aria-label="Role actions"
                              (click)="toggleMenu(role.uuid!, $event)"
                            >
                              <akd-icon name="more-horizontal" [size]="15" />
                            </button>
                            @if (menuFor() === role.uuid) {
                              <div class="menu__list" role="menu">
                                <button
                                  class="menu__item"
                                  type="button"
                                  role="menuitem"
                                  (click)="openRole(role)"
                                >
                                  <akd-icon name="pencil" [size]="14" />
                                  Edit role
                                </button>
                                <button
                                  class="menu__item menu__item--danger"
                                  type="button"
                                  role="menuitem"
                                  (click)="deleteRole(role)"
                                >
                                  <akd-icon name="trash-2" [size]="14" />
                                  Delete role
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
                  @for (role of customRoles(); track role.uuid) {
                    <option [value]="'custom:' + role.uuid">{{ role.name }} (custom)</option>
                  }
                </select>
              </div>
              <span class="akd-field__hint">
                Admin: full control of the team. Member: manages resources. Reviewer: sees PR
                previews only. Custom roles are defined in the Roles tab. Instance settings stay
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

      <akd-modal
        [open]="memberRoleOpen()"
        [title]="'Change role — ' + (roleMember()?.name ?? roleMember()?.email ?? '')"
        (closed)="memberRoleOpen.set(false)"
      >
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <form id="member-role-form" class="modal-stack" (ngSubmit)="saveMemberRole()">
          <div class="akd-field">
            <label class="akd-field__label" for="member-role">Role</label>
            <div class="akd-select">
              <select
                id="member-role"
                name="role"
                class="akd-input"
                [(ngModel)]="memberRole"
                [disabled]="busy()"
              >
                <option value="admin">admin</option>
                <option value="member">member</option>
                <option value="reviewer">reviewer</option>
                @for (role of customRoles(); track role.uuid) {
                  <option [value]="'custom:' + role.uuid">{{ role.name }} (custom)</option>
                }
              </select>
            </div>
            <span class="akd-field__hint">
              Built-in roles or one of this team's custom roles. <code>none</code> grants nothing
              team-wide — it is how somebody is restricted to the projects they are assigned (Scoped
              access tab).
            </span>
          </div>
        </form>
        <div modal-footer>
          <button
            class="akd-btn akd-btn--ghost"
            type="button"
            (click)="memberRoleOpen.set(false)"
            [disabled]="busy()"
          >
            Cancel
          </button>
          <button
            class="akd-btn akd-btn--primary"
            type="submit"
            form="member-role-form"
            [disabled]="busy()"
          >
            {{ busy() ? 'Saving…' : 'Save' }}
          </button>
        </div>
      </akd-modal>

      <akd-modal
        [open]="roleOpen()"
        [title]="editingRole() ? 'Edit custom role' : 'New custom role'"
        (closed)="roleOpen.set(false)"
      >
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        <form id="role-form" class="modal-stack" (ngSubmit)="saveRole()">
          <div class="akd-field">
            <label class="akd-field__label" for="role-name">Name</label>
            <input
              id="role-name"
              name="name"
              type="text"
              class="akd-input"
              [(ngModel)]="roleName"
              [disabled]="busy()"
              required
            />
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="role-desc">Description</label>
            <input
              id="role-desc"
              name="description"
              type="text"
              class="akd-input"
              [(ngModel)]="roleDescription"
              [disabled]="busy()"
            />
          </div>
          <div class="akd-field">
            <span class="akd-field__label">Permissions</span>
            <span class="akd-field__hint">
              Prerequisites are added automatically. Instance-wide permissions can never be granted.
            </span>
            <div class="composer">
              @for (group of permissionGroups(); track group.domain) {
                <fieldset class="composer__group">
                  <legend>{{ group.domain }}</legend>
                  @for (perm of group.permissions; track perm.permission) {
                    <label class="composer__perm">
                      <input
                        type="checkbox"
                        [checked]="selectedPerms().has(perm.permission)"
                        [disabled]="busy()"
                        (change)="togglePerm(perm)"
                      />
                      <span class="composer__action">{{ actionOf(perm.permission) }}</span>
                      @if (perm.prerequisites.length > 0) {
                        <span class="composer__prereq"
                          >needs {{ perm.prerequisites.join(', ') }}</span
                        >
                      }
                    </label>
                  }
                </fieldset>
              }
            </div>
          </div>
        </form>
        <div modal-footer>
          <button
            class="akd-btn akd-btn--ghost"
            type="button"
            (click)="roleOpen.set(false)"
            [disabled]="busy()"
          >
            Cancel
          </button>
          <button
            class="akd-btn akd-btn--primary"
            type="submit"
            form="role-form"
            [disabled]="busy() || !roleName.trim() || selectedPerms().size === 0"
          >
            {{ busy() ? 'Saving…' : editingRole() ? 'Save role' : 'Create role' }}
          </button>
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
      .card-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: var(--space-4) var(--space-5);
        border-bottom: 1px solid var(--border-2);
        font-weight: var(--weight-semibold);
      }
      .right {
        text-align: right;
      }
      .composer {
        display: grid;
        gap: var(--space-3);
        max-height: 340px;
        overflow-y: auto;
        padding: var(--space-2);
        border: 1px solid var(--border-2);
        border-radius: var(--radius-2);
        background: var(--bg-inset);
      }
      .composer__group {
        border: 0;
        margin: 0;
        padding: 0;
        display: grid;
        gap: 2px;
      }
      .composer__group legend {
        font-family: var(--font-mono);
        font-size: var(--text-2xs);
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: var(--text-3);
        padding: 0 0 2px;
      }
      .composer__perm {
        display: flex;
        align-items: center;
        gap: var(--space-2);
        padding: 2px var(--space-2);
        border-radius: var(--radius-2);
        cursor: pointer;
      }
      .composer__perm:hover {
        background: var(--bg-2);
      }
      .composer__action {
        font-family: var(--font-mono);
        font-size: var(--text-xs);
      }
      .composer__prereq {
        font-size: var(--text-2xs);
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
      .access-row {
        background: var(--bg-inset);
      }
      .access-scope {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--space-2);
      }
      /* Same reading as the resource-side review: one dense line, not a row of
         pills — the two views must not drift apart visually either. */
      .access-scope .caps {
        font-size: var(--text-xs);
        color: var(--text-2);
      }
      .access-scope .sep {
        margin: 0 2px;
        color: var(--text-3);
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

  /**
   * What this member reaches (ADR-046 §9) — the offboarding question, answered
   * where the person is, rather than by opening resources one by one.
   */
  protected async toggleAccess(member: TeamMember): Promise<void> {
    if (this.accessFor() === member.user_uuid) {
      this.accessFor.set(null);
      return;
    }
    this.accessFor.set(member.user_uuid);
    this.access.set([]);
    const teamUuid = this.api.currentUser()?.teamUuid ?? '';
    try {
      const page = await this.api.client().getMemberAccess(teamUuid, member.user_uuid);
      this.access.set(page.data);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.accessFor.set(null);
    }
  }

  protected readonly tab = signal<TeamTab>('members');
  protected readonly team = signal<Team | null>(null);
  protected readonly members = signal<TeamMember[]>([]);
  /** The member whose access rows are unfolded, and those rows. */
  protected readonly accessFor = signal<string | null>(null);
  protected readonly access = signal<MemberAccessEntry[]>([]);

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
  /** A system role, or `custom:<uuid>` for a custom role. */
  protected inviteRole = 'member';

  // --- roles & permissions ---
  protected readonly customRoles = signal<CustomRole[]>([]);
  protected readonly permissions = signal<PermissionEntry[]>([]);

  /** Composer catalogue grouped by domain, instance-scoped permissions dropped
   * (they can never belong to a custom role). */
  protected readonly permissionGroups = computed<PermissionGroup[]>(() => {
    const byDomain = new Map<string, PermissionEntry[]>();
    for (const perm of this.permissions()) {
      if (perm.instance_scoped) continue;
      const domain = perm.permission.split(':')[0];
      (byDomain.get(domain) ?? byDomain.set(domain, []).get(domain)!).push(perm);
    }
    return [...byDomain.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([domain, permissions]) => ({ domain, permissions }));
  });

  /** Change-member-role modal. */
  protected readonly memberRoleOpen = signal(false);
  protected readonly roleMember = signal<TeamMember | null>(null);
  /** Either a system role or `custom:<uuid>`. */
  protected memberRole = 'member';

  /** Create/edit custom-role modal. */
  protected readonly roleOpen = signal(false);
  protected readonly editingRole = signal<CustomRole | null>(null);
  protected readonly selectedPerms = signal<Set<string>>(new Set());
  protected roleName = '';
  protected roleDescription = '';

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
      const [team, members, invitations, roles, permissions] = await Promise.all([
        client.getTeam(teamUuid),
        client.listTeamMembers(teamUuid, { limit: 100 }),
        client.listTeamInvitations(teamUuid, { limit: 100 }),
        client.listTeamRoles(teamUuid, { limit: 100 }),
        client.listPermissions(),
      ]);
      this.team.set(team);
      this.members.set(members.data);
      this.invitations.set(invitations.data);
      this.customRoles.set(roles.data);
      this.permissions.set(permissions.data);
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
      const body: components['schemas']['InvitationCreate'] = this.inviteRole.startsWith('custom:')
        ? {
            email: this.inviteEmail.trim(),
            role: 'custom',
            custom_role_uuid: this.inviteRole.slice('custom:'.length),
            expires_in_hours: 168,
          }
        : {
            email: this.inviteEmail.trim(),
            role: this.inviteRole as 'admin' | 'member' | 'reviewer',
            expires_in_hours: 168,
          };
      const created = await this.api.client().createTeamInvitation(this.teamUuid, body);
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

  // --- member role change ------------------------------------------------------

  protected actionOf(permission: string): string {
    return permission.split(':')[1] ?? permission;
  }

  protected openMemberRole(member: TeamMember): void {
    this.error.set(null);
    this.roleMember.set(member);
    this.memberRole =
      member.role === 'custom' && member.custom_role_uuid
        ? `custom:${member.custom_role_uuid}`
        : member.role;
    this.memberRoleOpen.set(true);
  }

  protected async saveMemberRole(): Promise<void> {
    const member = this.roleMember();
    if (!this.teamUuid || !member) return;
    const body: components['schemas']['MemberRoleUpdate'] = this.memberRole.startsWith('custom:')
      ? { role: 'custom', custom_role_uuid: this.memberRole.slice('custom:'.length) }
      : { role: this.memberRole as 'admin' | 'member' | 'reviewer' };
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().updateTeamMember(this.teamUuid, member.user_uuid, body);
      this.memberRoleOpen.set(false);
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- custom roles ------------------------------------------------------------

  protected openRole(role: CustomRole | null): void {
    this.menuFor.set(null);
    this.error.set(null);
    this.editingRole.set(role);
    this.roleName = role?.name ?? '';
    this.roleDescription = role?.description ?? '';
    this.selectedPerms.set(new Set(role?.permissions ?? []));
    this.roleOpen.set(true);
  }

  /** Toggle a permission, pulling in (but never auto-removing) its prerequisites
   * — the server closes the set too, this is just immediate feedback. */
  protected togglePerm(perm: PermissionEntry): void {
    const next = new Set(this.selectedPerms());
    if (next.has(perm.permission)) {
      next.delete(perm.permission);
    } else {
      next.add(perm.permission);
      for (const prereq of perm.prerequisites) next.add(prereq);
    }
    this.selectedPerms.set(next);
  }

  protected async saveRole(): Promise<void> {
    if (!this.teamUuid || !this.roleName.trim() || this.selectedPerms().size === 0) return;
    const permissions = [...this.selectedPerms()];
    const description = this.roleDescription.trim() || null;
    this.busy.set(true);
    this.error.set(null);
    try {
      const editing = this.editingRole();
      if (editing?.uuid) {
        await this.api.client().updateTeamRole(this.teamUuid, editing.uuid, {
          name: this.roleName.trim(),
          description,
          permissions,
        });
      } else {
        await this.api
          .client()
          .createTeamRole(this.teamUuid, { name: this.roleName.trim(), description, permissions });
      }
      this.roleOpen.set(false);
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteRole(role: CustomRole): Promise<void> {
    this.menuFor.set(null);
    if (!this.teamUuid || !role.uuid) return;
    if (
      !confirm(
        `Delete the role "${role.name}"? Members carrying it fall back to their built-in role.`,
      )
    )
      return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteTeamRole(this.teamUuid, role.uuid);
      await this.load(this.teamUuid);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
