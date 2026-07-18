import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../core/api.service';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import type { components } from '../../api/schema';

type SharedVariable = components['schemas']['SharedVariable'];
type Scope = SharedVariable['scope'];

/**
 * Team-level settings (design kit: TeamSettingsScreen, General tab). Shared
 * variables are the real feature here: `{{team.KEY}}` references resolved at
 * deploy time. Connections live on the Notifications page; there is no team
 * deletion because the API does not expose one.
 */
@Component({
  selector: 'app-team-settings',
  standalone: true,
  imports: [FormsModule, CardComponent, IconComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Team settings</h1>
        @if (teamName()) {
          <span class="akd-badge akd-badge--mono">{{ teamName() }}</span>
        }
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="creating.set(!creating())">
          <akd-icon name="plus" [size]="15" />
          {{ creating() ? 'Cancel' : 'Add variable' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="sv-key">Key</label>
            <input
              id="sv-key"
              name="key"
              class="akd-input akd-input--mono"
              required
              pattern="[A-Za-z_][A-Za-z0-9_]*"
              placeholder="SENTRY_ORG"
              [(ngModel)]="key"
              [disabled]="busy()"
            />
            <span class="akd-field__hint"
              >Grammar [A-Za-z_][A-Za-z0-9_]* — referenced as {{ '{{' }}team.KEY{{ '}}' }}.</span
            >
          </div>
          <div class="akd-field">
            <label class="akd-field__label" for="sv-value">Value</label>
            <input
              id="sv-value"
              name="value"
              class="akd-input akd-input--mono"
              required
              [(ngModel)]="value"
              [disabled]="busy()"
            />
          </div>
          <label class="akd-check">
            <input type="checkbox" name="secret" [(ngModel)]="secret" [disabled]="busy()" />
            Secret — value redacted without read:sensitive
          </label>
          <div>
            <button
              class="akd-btn akd-btn--primary"
              type="submit"
              [disabled]="busy() || !key.trim()"
            >
              {{ busy() ? 'Creating…' : 'Create variable' }}
            </button>
          </div>
        </form>
      }

      <akd-card title="Shared variables" [padded]="false">
        @if (loading()) {
          <p class="akd-muted pad">Loading…</p>
        } @else if (variables().length === 0) {
          <p class="akd-muted pad">
            No shared variables yet — add one to reference it from any resource.
          </p>
        } @else {
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
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              @for (variable of variables(); track variable.uuid) {
                <tr>
                  <td class="akd-mono">{{ reference(variable) }}</td>
                  <td>
                    <span class="akd-badge akd-badge--mono">{{ variable.scope }}</span>
                  </td>
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
                      class="akd-iconbtn"
                      type="button"
                      [disabled]="busy()"
                      (click)="remove(variable)"
                      aria-label="Delete variable"
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

      <p class="footnote">
        References are interpolated at deploy time; an unknown reference stays verbatim in the
        container — visible, therefore diagnosable. Previews never receive shared secrets.
      </p>
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .create {
        margin-bottom: var(--space-5);
        max-width: 32rem;
      }
      .pad {
        padding: var(--space-5);
        margin: 0;
      }
      .footnote {
        margin-top: var(--space-3);
        font-size: var(--text-xs);
        color: var(--text-3);
      }
      akd-card {
        display: block;
      }
    `,
  ],
})
export class TeamSettingsComponent {
  private readonly api = inject(ApiService);

  protected readonly variables = signal<SharedVariable[]>([]);
  protected readonly teamName = signal<string | null>(null);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected key = '';
  protected value = '';
  protected secret = false;

  constructor() {
    void this.load();
  }

  protected reference(variable: SharedVariable): string {
    const scope: Scope = variable.scope;
    return `{{${scope}.${variable.key}}}`;
  }

  private async load(): Promise<void> {
    try {
      const [variables, teams] = await Promise.all([
        this.api.client().listSharedVariables({ limit: 100 }),
        this.api.client().listTeams({ limit: 1 }),
      ]);
      this.variables.set(variables.data);
      this.teamName.set(teams.data[0]?.name ?? null);
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
      this.creating.set(false);
      await this.load();
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
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
