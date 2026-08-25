import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type Model = components['schemas']['Model'];

// The Models section (ADR-080 §6): a transverse view — every model of the
// team, its GPU server, its status. Creation lives on its own page
// (/models/new), like an application.
@Component({
  selector: 'app-models',
  standalone: true,
  imports: [RouterLink, CardComponent, EmptyStateComponent, IconComponent, StatusBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Models</h1>
        @if (!loading()) {
          <span class="akd-badge akd-badge--mono">{{ models().length }}</span>
        }
        <span class="grow"></span>
        <a class="akd-btn akd-btn--primary" routerLink="/models/new">
          <akd-icon name="plus" [size]="15" />
          New model
        </a>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (models().length === 0) {
        <akd-empty-state
          icon="cpu"
          title="No models yet"
          message="Serve vLLM or SGLang on a GPU server — parameters typed, the full flag surface one list away."
        >
          <a class="akd-btn akd-btn--secondary" routerLink="/models/new">
            <akd-icon name="plus" [size]="15" />
            New model
          </a>
        </akd-empty-state>
      } @else {
        <akd-card [padded]="false">
          <table class="akd-table">
            <caption class="sr-only">
              Inference models of this team, with their GPU server and state
            </caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Engine</th>
                <th scope="col">Model</th>
                <th scope="col">Server</th>
                <th scope="col">Desired</th>
                <th scope="col">Observed</th>
              </tr>
            </thead>
            <tbody>
              @for (model of models(); track model.uuid) {
                <tr>
                  <td>
                    <a class="akd-mono" [routerLink]="['/models', model.uuid]">{{ model.name }}</a>
                  </td>
                  <td>
                    <span class="akd-badge akd-badge--accent akd-badge--mono">{{
                      model.engine
                    }}</span>
                    @if (model.modality === 'omni') {
                      <span class="akd-badge akd-badge--mono">omni</span>
                    }
                  </td>
                  <td class="akd-mono truncate">{{ model.model_id }}</td>
                  <td>
                    {{ model.server_name }}
                    @if (model.server_gpu_name) {
                      <span class="akd-muted">— {{ model.server_gpu_name }}</span>
                    }
                  </td>
                  <td><akd-status-badge domain="resource" [state]="model.status" /></td>
                  <td>
                    <akd-status-badge
                      domain="resource"
                      [state]="model.observed_status ?? 'unknown'"
                    />
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </akd-card>
      }
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .truncate {
        max-width: 18rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
    `,
  ],
})
export class ModelsComponent {
  private readonly api = inject(ApiService);

  protected readonly models = signal<Model[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);

  constructor() {
    // The pre-creation-page deep link (?create=1&project=&environment=) still
    // reaches the dedicated page, anchor preserved.
    const route = inject(ActivatedRoute);
    const router = inject(Router);
    const params = route.snapshot.queryParamMap;
    if (params.get('create')) {
      void router.navigate(['/models/new'], {
        replaceUrl: true,
        queryParams: {
          project: params.get('project'),
          environment: params.get('environment'),
        },
      });
      return;
    }
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const models = await fetchAll((cursor) =>
        this.api.client().listModels({ limit: 100, cursor }),
      );
      this.models.set(models);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }
}
