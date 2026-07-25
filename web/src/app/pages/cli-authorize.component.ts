import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { ApiService } from '../core/api.service';

type Team = { uuid: string; name: string };

/**
 * CLI login consent page (ADR-031): reached in the browser as
 * /cli/authorize?request_id=…. The panel session (any login method) is already
 * established by the route guard; here the user confirms the confirmation code
 * shown in their terminal, picks a team and permissions, and approves. The
 * verifier never reaches this page — only the request id and the code do.
 */
@Component({
  selector: 'app-cli-authorize',
  standalone: true,
  imports: [FormsModule, CardComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'akd-page' },
  template: `
    <div class="wrap">
      <akd-card title="Authorize the AkerDock CLI">
        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (done()) {
          <p class="akd-muted">
            Approved. Return to your terminal — the CLI will finish signing in.
          </p>
        } @else if (loaded()) {
          <p class="akd-muted">
            A command-line client on <strong>{{ clientName() || 'an unknown machine' }}</strong>
            is requesting access. Confirm the code shown in your terminal matches:
          </p>
          <p class="code">{{ userCode() }}</p>

          <label class="akd-field__label" for="team">Team</label>
          <div class="akd-select">
            <select id="team" class="akd-input" [(ngModel)]="teamUuid">
              @for (t of teams(); track t.uuid) {
                <option [value]="t.uuid">{{ t.name }}</option>
              }
            </select>
          </div>

          <p class="akd-field__label">Permissions</p>
          @for (p of permissionChoices; track p.key) {
            <label class="akd-check">
              <input type="checkbox" [(ngModel)]="p.checked" [disabled]="p.locked" />
              {{ p.key }} — {{ p.hint }}
            </label>
          }

          <div class="actions">
            <button class="akd-btn akd-btn--primary" type="button" [disabled]="busy()" (click)="approve()">
              Approve access
            </button>
          </div>
          <p class="akd-muted small">
            Approving grants API access to whoever started this request. Only approve a code you
            recognize.
          </p>
        } @else {
          <p class="akd-muted">Loading…</p>
        }
      </akd-card>
    </div>
  `,
  styles: [
    `
      .wrap {
        max-width: 32rem;
        margin: 8vh auto;
      }
      .code {
        font-family: var(--font-mono);
        font-size: var(--text-xl);
        letter-spacing: 0.3em;
        text-align: center;
        padding: var(--space-3);
        background: var(--surface-2);
        border-radius: var(--radius-md);
      }
      .actions {
        margin-top: var(--space-4);
      }
      .small {
        font-size: var(--text-xs);
        margin-top: var(--space-3);
      }
    `,
  ],
})
export class CliAuthorizeComponent {
  private readonly route = inject(ActivatedRoute);
  private readonly api = inject(ApiService);

  protected readonly loaded = signal(false);
  protected readonly done = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);
  protected readonly userCode = signal('');
  protected readonly clientName = signal('');
  protected readonly teams = signal<Team[]>([]);

  protected teamUuid = '';
  protected permissionChoices = [
    { key: 'read', hint: 'view resources', checked: true, locked: true },
    { key: 'write', hint: 'create and edit, open shells and tunnels', checked: true, locked: false },
    { key: 'deploy', hint: 'trigger deployments', checked: false, locked: false },
    { key: 'read:sensitive', hint: 'reveal secrets', checked: false, locked: false },
  ];

  private get requestId(): string {
    return this.route.snapshot.queryParamMap.get('request_id') ?? '';
  }

  constructor() {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const [meta, teamsPage] = await Promise.all([
        fetch('/auth/cli/request?request_id=' + encodeURIComponent(this.requestId), {
          credentials: 'same-origin',
        }).then((r) => (r.ok ? r.json() : Promise.reject(new Error('unknown or expired request')))),
        this.api.client().listTeams(),
      ]);
      this.userCode.set(meta.user_code ?? '');
      this.clientName.set(meta.name ?? '');
      this.teams.set(teamsPage.data as Team[]);
      if (teamsPage.data.length > 0) this.teamUuid = teamsPage.data[0].uuid;
      this.loaded.set(true);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
      this.loaded.set(true);
    }
  }

  protected async approve(): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      const permissions = this.permissionChoices.filter((p) => p.checked).map((p) => p.key);
      const resp = await fetch('/auth/cli/approve', {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': readCookie('akerdock_csrf'),
        },
        body: JSON.stringify({ request_id: this.requestId, team_uuid: this.teamUuid, permissions }),
      });
      if (!resp.ok && resp.status !== 204) {
        const body = await resp.json().catch(() => ({}));
        throw new Error(body.message ?? 'approval failed');
      }
      this.done.set(true);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.busy.set(false);
    }
  }
}

function readCookie(name: string): string {
  const match = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
  return match ? decodeURIComponent(match[1]) : '';
}
