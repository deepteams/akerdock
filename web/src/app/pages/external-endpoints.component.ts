import { ChangeDetectionStrategy, Component, effect, inject, signal } from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';
import { ApiService } from '../core/api.service';
import { AuditLogComponent, type AuditFetch } from './audit-log.component';
import { TunnelEndpointsTabComponent } from './tunnels/endpoints-tab.component';
import { TunnelSessionsTabComponent } from './tunnels/sessions-tab.component';

type TunnelTab = 'endpoints' | 'sessions' | 'audit';

const TABS: { id: TunnelTab; label: string }[] = [
  { id: 'endpoints', label: 'Configured' },
  { id: 'sessions', label: 'Open tunnels' },
  { id: 'audit', label: 'Audit log' },
];

/**
 * Tunnels (ADR-032/ADR-045) in the three states an operator asks about: what is
 * **declared**, what is **open right now**, and what **happened** — declarations,
 * access grants and every tunnel opened or cut.
 *
 * The audit tab is the same viewer as the team trail, narrowed to this feature's
 * action prefixes. Narrowing on the server rather than in the page matters: a
 * client-side filter over a page of the global trail would show a handful of
 * rows and silently hide the rest behind pagination.
 */
@Component({
  selector: 'app-external-endpoints',
  standalone: true,
  imports: [AuditLogComponent, TunnelEndpointsTabComponent, TunnelSessionsTabComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Tunnels</h1>
      </header>

      <div class="akd-tabs" role="tablist">
        @for (tab of tabs; track tab.id) {
          <button
            class="akd-tab"
            role="tab"
            [class.akd-tab--active]="active() === tab.id"
            [attr.aria-selected]="active() === tab.id"
            (click)="select(tab.id)"
          >
            {{ tab.label }}
          </button>
        }
      </div>

      <div class="pane">
        @switch (active()) {
          @case ('endpoints') {
            <app-tunnel-endpoints-tab />
          }
          @case ('sessions') {
            <app-tunnel-sessions-tab />
          }
          @case ('audit') {
            <akd-audit-log [fetch]="fetchAudit" exportName="tunnel-audit" />
          }
        }
      </div>
    </div>
  `,
})
export class ExternalEndpointsComponent {
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  private readonly api = inject(ApiService);

  protected readonly tabs = TABS;
  protected readonly active = signal<TunnelTab>('endpoints');

  constructor() {
    const requested = this.route.snapshot.queryParamMap.get('tab') as TunnelTab | null;
    if (requested && TABS.some((t) => t.id === requested)) this.active.set(requested);
    effect(() => {
      void this.router.navigate([], { queryParams: { tab: this.active() }, replaceUrl: true });
    });
  }

  protected select(tab: TunnelTab): void {
    this.active.set(tab);
  }

  /**
   * The feature's whole trail, not one action at a time: declarations and
   * updates (`external-endpoint.*`, including grants and revocations) plus every
   * tunnel opened and closed (`port-forward.*`). The operator's question is
   * "who reached production, when, and with whose blessing" — that story spans
   * both prefixes.
   */
  protected readonly fetchAudit: AuditFetch = (query) => {
    const teamUuid = this.api.currentUser()?.teamUuid ?? '';
    return this.api.client().listTeamAudit(teamUuid, {
      ...query,
      action_prefix: ['external-endpoint.', 'port-forward.'],
    });
  };
}
