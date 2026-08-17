import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { EmptyStateComponent } from '../../ui/empty-state/empty-state.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ApiService } from '../core/api.service';
import { fetchAll } from '../core/pagination';
import type { components } from '../../api/schema';

type Model = components['schemas']['Model'];
type Project = components['schemas']['Project'];
type Environment = components['schemas']['Environment'];
type Server = components['schemas']['Server'];
type EngineFlag = components['schemas']['EngineFlag'];
type HubModel = components['schemas']['HubModel'];

// The Models section (ADR-080 §6): a transverse view — every model of the
// team, its GPU server, its status — and a creation flow that starts from
// the MODEL (engine → Hub search → GPU server → typed parameters), the
// project/environment anchor coming last, defaulted. The serve command is a
// first-class representation here: previewed by the deployment's own
// renderer, and importable by paste (§3bis).
@Component({
  selector: 'app-models',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    CardComponent,
    EmptyStateComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      <header class="akd-bar">
        <h1>Models</h1>
        @if (!loading()) {
          <span class="akd-badge akd-badge--mono">{{ models().length }}</span>
        }
        <span class="grow"></span>
        <button class="akd-btn akd-btn--primary" type="button" (click)="toggleCreate()">
          <akd-icon name="plus" [size]="15" />
          {{ creating() ? 'Cancel' : 'New model' }}
        </button>
      </header>

      @if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      }

      @if (creating()) {
        <form class="akd-card create" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="mo-paste">Paste a serve command (optional)</label>
            <textarea
              id="mo-paste"
              name="paste"
              class="akd-input akd-input--mono"
              rows="2"
              placeholder="vllm serve org/model --max-model-len 8192 …"
              [(ngModel)]="pasted"
              [disabled]="busy()"
            ></textarea>
            <span class="akd-field__hint">
              Any vLLM or SGLang command from a playbook or a blog fills the form below —
              platform-managed flags (port, api-key…) are taken over, with a notice.
            </span>
            <div>
              <button
                class="akd-btn akd-btn--secondary"
                type="button"
                (click)="importCommand()"
                [disabled]="busy() || !pasted.trim()"
              >
                Fill the form from this command
              </button>
            </div>
            @for (notice of importNotices(); track notice) {
              <span class="akd-field__hint">{{ notice }}</span>
            }
          </div>

          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="mo-engine">Engine</label>
              <div class="akd-select">
                <select
                  id="mo-engine"
                  name="engine"
                  class="akd-input"
                  [(ngModel)]="engine"
                  [disabled]="busy()"
                >
                  <option value="vllm">vLLM</option>
                  <option value="sglang">SGLang</option>
                </select>
              </div>
            </div>
            <div class="akd-field grow">
              <label class="akd-field__label" for="mo-name">Name</label>
              <input
                id="mo-name"
                name="name"
                class="akd-input"
                required
                [(ngModel)]="name"
                [disabled]="busy()"
              />
            </div>
          </div>

          <div class="akd-field">
            <label class="akd-field__label" for="mo-model">Hugging Face model</label>
            <input
              id="mo-model"
              name="modelId"
              class="akd-input akd-input--mono"
              required
              placeholder="org/model — type to search the Hub"
              [(ngModel)]="modelId"
              (ngModelChange)="onModelInput($event)"
              [disabled]="busy()"
              autocomplete="off"
            />
            @if (suggestions().length > 0) {
              <ul class="suggestions" role="listbox">
                @for (hit of suggestions(); track hit.id) {
                  <li>
                    <button type="button" class="suggestion" (click)="pickSuggestion(hit)">
                      <span class="akd-mono">{{ hit.id }}</span>
                      @if (hit.gated) {
                        <span class="akd-badge">gated</span>
                      }
                      @if (hit.downloads != null) {
                        <span class="akd-muted">{{ hit.downloads }} downloads</span>
                      }
                    </button>
                  </li>
                }
              </ul>
            }
            <span class="akd-field__hint">
              The search suggests; it never constrains — any Hub reference works.
            </span>
          </div>

          <div class="akd-field">
            <label class="akd-field__label" for="mo-server">GPU server</label>
            <div class="akd-select">
              <select
                id="mo-server"
                name="server"
                class="akd-input"
                [(ngModel)]="serverUuid"
                [disabled]="busy()"
              >
                <option value="" disabled>
                  {{ gpuServers().length ? 'Choose a GPU server…' : 'No server with an observed GPU' }}
                </option>
                @for (server of gpuServers(); track server.uuid) {
                  <option [value]="server.uuid">
                    {{ server.name }} — {{ server.gpu_name }}
                  </option>
                }
              </select>
            </div>
            <span class="akd-field__hint">
              Only servers whose validation observed a GPU appear (ADR-079).
            </span>
          </div>

          <div class="row">
            <div class="akd-field">
              <label class="akd-field__label" for="mo-quant">Quantization</label>
              <input
                id="mo-quant"
                name="quantization"
                class="akd-input akd-input--mono"
                placeholder="engine default"
                [(ngModel)]="quantization"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="mo-maxlen">Max model len</label>
              <input
                id="mo-maxlen"
                name="maxModelLen"
                class="akd-input"
                type="number"
                min="1"
                placeholder="auto"
                [(ngModel)]="maxModelLen"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="mo-mem">Memory fraction</label>
              <input
                id="mo-mem"
                name="memoryFraction"
                class="akd-input"
                type="number"
                step="0.01"
                min="0.01"
                max="1"
                placeholder="engine default"
                [(ngModel)]="memoryFraction"
                [disabled]="busy()"
              />
            </div>
            <div class="akd-field">
              <label class="akd-field__label" for="mo-tp">Tensor parallel</label>
              <input
                id="mo-tp"
                name="tensorParallel"
                class="akd-input"
                type="number"
                min="1"
                [(ngModel)]="tensorParallel"
                [disabled]="busy()"
              />
            </div>
          </div>

          <div class="akd-field">
            <span class="akd-field__label">Engine flags</span>
            @for (flag of flags(); track $index; let i = $index) {
              <div class="row">
                <input
                  class="akd-input akd-input--mono"
                  [name]="'flag-' + i"
                  placeholder="--a-flag"
                  [ngModel]="flag.flag"
                  (ngModelChange)="setFlag(i, 'flag', $event)"
                  [disabled]="busy()"
                />
                <input
                  class="akd-input akd-input--mono"
                  [name]="'flagv-' + i"
                  placeholder="value (empty = boolean)"
                  [ngModel]="flag.value ?? ''"
                  (ngModelChange)="setFlag(i, 'value', $event)"
                  [disabled]="busy()"
                />
                <button
                  class="akd-iconbtn"
                  type="button"
                  aria-label="Remove flag"
                  (click)="removeFlag(i)"
                  [disabled]="busy()"
                >
                  <akd-icon name="x" [size]="14" />
                </button>
              </div>
            }
            <div>
              <button
                class="akd-btn akd-btn--secondary"
                type="button"
                (click)="addFlag()"
                [disabled]="busy()"
              >
                <akd-icon name="plus" [size]="14" />
                Add a flag
              </button>
            </div>
            <span class="akd-field__hint">
              The whole upstream surface, in order. Platform-managed flags (--port, --api-key,
              --host, --download-dir, --hf-token) and the typed knobs above are refused here.
            </span>
          </div>

          <div class="row">
            <div class="akd-field grow">
              <label class="akd-field__label" for="mo-project">Project</label>
              <div class="akd-select">
                <select
                  id="mo-project"
                  name="project"
                  class="akd-input"
                  [(ngModel)]="projectUuid"
                  (ngModelChange)="onProjectChange($event)"
                  [disabled]="busy()"
                >
                  <option value="" disabled>Choose a project…</option>
                  @for (project of projects(); track project.uuid) {
                    <option [value]="project.uuid">{{ project.name }}</option>
                  }
                </select>
              </div>
            </div>
            <div class="akd-field grow">
              <label class="akd-field__label" for="mo-environment">Environment</label>
              <div class="akd-select">
                <select
                  id="mo-environment"
                  name="environment"
                  class="akd-input"
                  [(ngModel)]="environmentUuid"
                  [disabled]="busy() || !projectUuid"
                >
                  <option value="" disabled>
                    {{ projectUuid ? 'Choose an environment…' : 'Pick a project first' }}
                  </option>
                  @for (env of environments(); track env.uuid) {
                    <option [value]="env.uuid">{{ env.name }}</option>
                  }
                </select>
              </div>
            </div>
          </div>

          @if (preview(); as command) {
            <div class="akd-field">
              <span class="akd-field__label">Command</span>
              <pre class="akd-log command">{{ command }}</pre>
            </div>
          }

          <div class="row">
            <button
              class="akd-btn akd-btn--secondary"
              type="button"
              (click)="showCommand()"
              [disabled]="busy() || !modelId.trim()"
            >
              Show the command
            </button>
            <label class="akd-check">
              <input
                type="checkbox"
                name="instantStart"
                [(ngModel)]="instantStart"
                [disabled]="busy()"
              />
              Start immediately (downloads and loads the weights)
            </label>
            <span class="grow"></span>
            <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy() || !valid()">
              {{ busy() ? 'Creating…' : 'Create model' }}
            </button>
          </div>
        </form>
      }

      @if (loading()) {
        <p class="akd-muted">Loading…</p>
      } @else if (models().length === 0) {
        <akd-empty-state
          icon="cpu"
          title="No models yet"
          message="Serve vLLM or SGLang on a GPU server — parameters typed, the full flag surface one list away."
        >
          @if (!creating()) {
            <button class="akd-btn akd-btn--secondary" type="button" (click)="toggleCreate()">
              <akd-icon name="plus" [size]="15" />
              New model
            </button>
          }
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
                  </td>
                  <td class="akd-mono truncate">{{ model.model_id }}</td>
                  <td>
                    {{ model.server_name }}
                    @if (model.server_gpu_name) {
                      <span class="akd-muted">— {{ model.server_gpu_name }}</span>
                    }
                  </td>
                  <td><akd-status-badge domain="resource" [state]="model.status" /></td>
                  <td><akd-status-badge domain="resource" [state]="model.observed_status ?? 'unknown'" /></td>
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
      .create {
        margin-bottom: var(--space-5);
        max-width: 44rem;
      }
      .row {
        display: flex;
        gap: var(--space-3);
        align-items: center;
      }
      .row .akd-field {
        flex: 1;
      }
      .suggestions {
        list-style: none;
        margin: var(--space-2) 0 0;
        padding: 0;
        display: grid;
        gap: 2px;
      }
      .suggestion {
        display: flex;
        gap: var(--space-2);
        align-items: baseline;
        width: 100%;
        text-align: left;
        background: none;
        border: 0;
        padding: var(--space-1) var(--space-2);
        cursor: pointer;
        color: inherit;
      }
      .suggestion:hover {
        background: var(--surface-2);
      }
      .command {
        white-space: pre-wrap;
        word-break: break-all;
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
  protected readonly projects = signal<Project[]>([]);
  protected readonly environments = signal<Environment[]>([]);
  protected readonly servers = signal<Server[]>([]);
  protected readonly suggestions = signal<HubModel[]>([]);
  protected readonly importNotices = signal<string[]>([]);
  protected readonly preview = signal<string | null>(null);
  protected readonly flags = signal<EngineFlag[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);

  // Only servers whose validation observed a GPU may host a model (ADR-079).
  protected readonly gpuServers = computed(() =>
    this.servers().filter((server) => !!server.gpu_name),
  );

  protected engine: 'vllm' | 'sglang' = 'vllm';
  protected name = '';
  protected modelId = '';
  protected pasted = '';
  protected quantization = '';
  protected maxModelLen: number | null = null;
  protected memoryFraction: number | null = null;
  protected tensorParallel = 1;
  protected serverUuid = '';
  protected projectUuid = '';
  protected environmentUuid = '';
  protected instantStart = false;

  private searchTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    void this.load();
    // The environment's "New resource" menu lands here with
    // ?create=1&project=&environment= — the model is then created where the
    // user already was, the anchor pre-filled instead of defaulted.
    const params = inject(ActivatedRoute).snapshot.queryParamMap;
    const project = params.get('project');
    const environment = params.get('environment');
    if (params.get('create')) {
      this.creating.set(true);
      void this.loadSelectors().then(async () => {
        if (!project) return;
        this.projectUuid = project;
        await this.onProjectChange(project);
        if (environment && this.environments().some((env) => env.uuid === environment)) {
          this.environmentUuid = environment;
        }
      });
    }
  }

  protected valid(): boolean {
    return !!(
      this.name.trim() &&
      this.modelId.trim() &&
      this.serverUuid &&
      this.projectUuid &&
      this.environmentUuid
    );
  }

  private async load(): Promise<void> {
    try {
      const models = await fetchAll((cursor) => this.api.client().listModels({ limit: 100, cursor }));
      this.models.set(models);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected toggleCreate(): void {
    this.creating.set(!this.creating());
    if (this.creating()) void this.loadSelectors();
  }

  private async loadSelectors(): Promise<void> {
    try {
      const [projects, servers] = await Promise.all([
        fetchAll((cursor) => this.api.client().listProjects({ limit: 100, cursor })),
        fetchAll((cursor) => this.api.client().listServers({ limit: 100, cursor })),
      ]);
      this.projects.set(projects);
      this.servers.set(servers);
      // The anchor comes LAST in this flow (ADR-080 §6): default it so the
      // model operator only touches it when they mean to.
      if (!this.projectUuid && projects.length > 0) {
        this.projectUuid = projects[0].uuid ?? '';
        await this.onProjectChange(this.projectUuid);
      }
      const gpu = this.gpuServers();
      if (!this.serverUuid && gpu.length === 1) {
        this.serverUuid = gpu[0].uuid ?? '';
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async onProjectChange(projectUuid: string): Promise<void> {
    this.environmentUuid = '';
    this.environments.set([]);
    if (!projectUuid) return;
    try {
      const environments = await fetchAll((cursor) =>
        this.api.client().listEnvironments(projectUuid, { limit: 100, cursor }),
      );
      this.environments.set(environments);
      if (environments.length === 1) {
        this.environmentUuid = environments[0].uuid ?? '';
      }
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /** Debounced Hub search — suggestions fill the field, never constrain it. */
  protected onModelInput(value: string): void {
    this.preview.set(null);
    if (this.searchTimer) clearTimeout(this.searchTimer);
    const query = value.trim();
    if (query.length < 2) {
      this.suggestions.set([]);
      return;
    }
    this.searchTimer = setTimeout(() => {
      void this.api
        .client()
        .searchModelHub({ q: query })
        .then((res) => this.suggestions.set(res.data))
        .catch(() => this.suggestions.set([]));
    }, 300);
  }

  protected pickSuggestion(hit: HubModel): void {
    this.modelId = hit.id;
    this.suggestions.set([]);
  }

  protected addFlag(): void {
    this.flags.set([...this.flags(), { flag: '' }]);
  }

  protected removeFlag(index: number): void {
    this.flags.set(this.flags().filter((_, i) => i !== index));
  }

  protected setFlag(index: number, key: 'flag' | 'value', value: string): void {
    this.flags.set(
      this.flags().map((flag, i) => (i === index ? { ...flag, [key]: value } : flag)),
    );
    this.preview.set(null);
  }

  private cleanFlags(): EngineFlag[] {
    return this.flags()
      .filter((flag) => flag.flag.trim() !== '')
      .map((flag) => ({
        flag: flag.flag.trim(),
        ...(flag.value?.trim() ? { value: flag.value.trim() } : {}),
      }));
  }

  /** The import half of §3bis: a pasted command fills the form. */
  protected async importCommand(): Promise<void> {
    this.error.set(null);
    this.importNotices.set([]);
    try {
      const parsed = await this.api.client().parseModelCommand({ command: this.pasted });
      this.engine = parsed.engine;
      this.modelId = parsed.model_id;
      this.quantization = parsed.quantization ?? '';
      this.maxModelLen = parsed.max_model_len ?? null;
      this.memoryFraction = parsed.memory_fraction ?? null;
      this.tensorParallel = parsed.tensor_parallel_size ?? 1;
      this.flags.set(parsed.engine_flags);
      this.importNotices.set(parsed.notices);
      this.preview.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /** The export half: THE renderer, on the unsaved form. */
  protected async showCommand(): Promise<void> {
    this.error.set(null);
    try {
      const res = await this.api.client().previewModelCommand({
        engine: this.engine,
        model_id: this.modelId.trim(),
        served_model_name: null,
        quantization: this.quantization.trim() || null,
        max_model_len: this.maxModelLen,
        tensor_parallel_size: this.tensorParallel > 1 ? this.tensorParallel : null,
        memory_fraction: this.memoryFraction,
        engine_flags: this.cleanFlags(),
      });
      this.preview.set(res.command);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async create(): Promise<void> {
    if (this.busy() || !this.valid()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createModel({
        name: this.name.trim(),
        engine: this.engine,
        model_id: this.modelId.trim(),
        quantization: this.quantization.trim() || null,
        max_model_len: this.maxModelLen,
        tensor_parallel_size: this.tensorParallel,
        memory_fraction: this.memoryFraction,
        engine_flags: this.cleanFlags(),
        project_uuid: this.projectUuid,
        environment_uuid: this.environmentUuid,
        server_uuid: this.serverUuid,
        instant_start: this.instantStart,
      });
      this.name = '';
      this.modelId = '';
      this.pasted = '';
      this.flags.set([]);
      this.preview.set(null);
      this.creating.set(false);
      await this.load();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
