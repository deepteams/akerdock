import { ChangeDetectionStrategy, Component, effect, inject, signal } from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';
import { ApiService } from '../core/api.service';
import { AuditLogComponent, type AuditFetch } from './audit-log.component';
import { IngressEndpointsTabComponent } from './ingress/endpoints-tab.component';
import { IngressSessionsTabComponent } from './ingress/sessions-tab.component';

type IngressTab = 'endpoints' | 'sessions' | 'audit';

const TABS: { id: IngressTab; label: string }[] = [
  { id: 'endpoints', label: 'Configured' },
  { id: 'sessions', label: 'Live tunnels' },
  { id: 'audit', label: 'Audit log' },
];

/**
 * Ingress endpoints (ADR-060) — the mirror of Tunnels. A Tunnel reaches OUT
 * from a laptop to a declared destination; an ingress endpoint relays IN, from
 * a stable public URL to a service running on a developer's machine.
 *
 * Same three states an operator asks about: what is **declared**, what is
 * **relaying right now**, and what **happened**. The audit tab is the team
 * trail narrowed on the server to this feature's action prefixes.
 */
@Component({
  selector: 'app-ingress',
  standalone: true,
  imports: [AuditLogComponent, IngressEndpointsTabComponent, IngressSessionsTabComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Ingress</h1>
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
            <app-ingress-endpoints-tab />
          }
          @case ('sessions') {
            <app-ingress-sessions-tab />
          }
          @case ('audit') {
            <akd-audit-log [fetch]="fetchAudit" exportName="ingress-audit" />
          }
        }
      </div>
    </div>
  `,
})
export class IngressComponent {
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  private readonly api = inject(ApiService);

  protected readonly tabs = TABS;
  protected readonly active = signal<IngressTab>('endpoints');

  constructor() {
    const requested = this.route.snapshot.queryParamMap.get('tab') as IngressTab | null;
    if (requested && TABS.some((t) => t.id === requested)) this.active.set(requested);
    effect(() => {
      void this.router.navigate([], { queryParams: { tab: this.active() }, replaceUrl: true });
    });
  }

  protected select(tab: IngressTab): void {
    this.active.set(tab);
  }

  /** Declarations, access-mode changes and every attach opened or cut. */
  protected readonly fetchAudit: AuditFetch = (query) => {
    const teamUuid = this.api.currentUser()?.teamUuid ?? '';
    return this.api.client().listTeamAudit(teamUuid, {
      ...query,
      action_prefix: ['ingress-endpoint.', 'ingress-tunnel.'],
    });
  };
}
