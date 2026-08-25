import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import {
  ActionsMenuComponent,
  type ActionItem,
} from '../../ui/actions-menu/actions-menu.component';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiError } from '../../api/client';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Model = components['schemas']['Model'];
type EngineFlag = components['schemas']['EngineFlag'];
type EnvVar = components['schemas']['EnvironmentVariable'];
type LogLine = components['schemas']['LogLine'];
type TabId = 'overview' | 'settings' | 'logs' | 'envs';

// One model (ADR-080): lifecycle with the soft occupied-GPU guard — a 409
// names the running neighbour and the confirm IS the swap —, the serve
// command by the deployment's own renderer (masked, revealed under
// models:credentials), the managed key, and the typed settings.
/** Split a space/comma-separated domains input into API elements. */
function splitDomains(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((d) => d.trim())
    .filter(Boolean);
}

@Component({
  selector: 'app-model-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    ActionsMenuComponent,
    CardComponent,
    IconComponent,
    StatusBadgeComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-page">
      @if (model(); as mo) {
        <header class="akd-bar">
          <a class="akd-iconbtn" routerLink="/models" aria-label="Back to models">
            <akd-icon name="arrow-left" [size]="16" />
          </a>
          <h1>{{ mo.name }}</h1>
          <span class="akd-badge akd-badge--accent akd-badge--mono">{{ mo.engine }}</span>
          @if (mo.modality === 'omni') {
            <span class="akd-badge akd-badge--mono">omni</span>
          }
          <akd-status-badge domain="resource" [state]="mo.status" />
          <akd-status-badge domain="resource" [state]="mo.observed_status ?? 'unknown'" />
          <span class="grow"></span>
          <akd-actions-menu
            [items]="actions()"
            [disabled]="busy()"
            (selected)="runAction($event)"
          />
        </header>

        @if (mo.active_job; as job) {
          <div class="jobbanner" role="status">
            <span>
              <span class="akd-mono">{{ job.job_type }}</span>
              @if (job.cancel_requested_at) {
                is stopping — it ends at its next checkpoint
              } @else {
                is {{ job.status }}
              }
              —
              <a [routerLink]="['/jobs', job.uuid]">follow the job</a>
            </span>
            <span class="grow"></span>
            @if (!job.cancel_requested_at) {
              <button
                class="akd-btn akd-btn--secondary akd-btn--sm"
                type="button"
                (click)="cancelActiveJob(job.uuid)"
                [disabled]="busy()"
              >
                Cancel
              </button>
            }
          </div>
        }

        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (notice(); as message) {
          <p class="akd-muted" role="status">{{ message }}</p>
        }

        <nav class="akd-tabs" role="tablist" aria-label="Model sections">
          @for (t of tabs(); track t.id) {
            <button
              type="button"
              class="akd-tab"
              role="tab"
              [class.akd-tab--active]="tab() === t.id"
              [attr.aria-selected]="tab() === t.id"
              (click)="selectTab(t.id)"
            >
              {{ t.label }}
            </button>
          }
        </nav>

        @switch (tab()) {
          @case ('overview') {
            <akd-card title="Endpoint">
              <p class="akd-muted intro">
                OpenAI-compatible, on the server's LAN address — protected by the managed API key
                the engine itself enforces. A stopped model keeps its endpoint and its key; the
                weights stay in the server's shared cache, so a resume reloads without
                re-downloading. Add a public domain in Settings to reach it beyond the LAN.
              </p>
              @for (url of publicUrls(); track url) {
                <div class="row">
                  <code class="akd-mono">{{ url }}</code>
                  <button
                    class="akd-iconbtn"
                    type="button"
                    aria-label="Copy the public URL"
                    (click)="copy(url)"
                  >
                    <akd-icon name="copy" [size]="14" />
                  </button>
                  <span class="akd-muted">public — HTTPS via the server's proxy</span>
                </div>
              }
              <div class="row">
                <code class="akd-mono">{{ mo.endpoint }}</code>
                <button
                  class="akd-iconbtn"
                  type="button"
                  aria-label="Copy the endpoint"
                  (click)="copy(mo.endpoint ?? '')"
                >
                  <akd-icon name="copy" [size]="14" />
                </button>
                <span class="akd-muted">
                  {{ mo.server_name }}
                  @if (mo.server_gpu_name) {
                    — {{ mo.server_gpu_name }}
                  }
                </span>
              </div>
              <div class="row">
                @if (apiKey(); as key) {
                  <code class="akd-mono">{{ key }}</code>
                  <button
                    class="akd-iconbtn"
                    type="button"
                    aria-label="Copy the API key"
                    (click)="copy(key)"
                  >
                    <akd-icon name="copy" [size]="14" />
                  </button>
                } @else {
                  <button
                    class="akd-btn akd-btn--secondary"
                    type="button"
                    (click)="revealKey()"
                    [disabled]="busy()"
                  >
                    Reveal the API key
                  </button>
                }
              </div>
            </akd-card>

            <akd-card title="Serve command">
              <p class="akd-muted intro">
                Rendered by the exact code the deployment runs — copy it, keep it in a note, paste
                it back into the creation form to clone this configuration.
              </p>
              @if (command(); as cmd) {
                <pre class="akd-log command">{{ cmd }}</pre>
                <div class="row">
                  <button class="akd-btn akd-btn--secondary" type="button" (click)="copy(cmd)">
                    <akd-icon name="copy" [size]="14" />
                    Copy
                  </button>
                  @if (commandMasked()) {
                    <button
                      class="akd-btn akd-btn--secondary"
                      type="button"
                      (click)="loadCommand(true)"
                      [disabled]="busy()"
                    >
                      Reveal the key in the command
                    </button>
                  }
                </div>
              } @else {
                <p class="akd-muted">Loading…</p>
              }
            </akd-card>
          }
          @case ('settings') {
            <akd-card title="Settings">
              <p class="akd-muted intro">
                Engine, server and port are immutable. Changes apply at the next start — the serve
                flags are read once, when the process starts.
              </p>
              <form class="form" (ngSubmit)="save()">
                <div class="akd-field">
                  <label class="akd-field__label" for="md-domains">Public domains</label>
                  <input
                    id="md-domains"
                    name="domains"
                    class="akd-input akd-input--mono"
                    placeholder="llm.service.example.com — empty = LAN only"
                    autocomplete="off"
                    [(ngModel)]="domains"
                    [disabled]="busy()"
                  />
                  <span class="akd-field__hint">
                    Routed with HTTPS by the server's proxy — through its edge relay when the server
                    is LAN-only. Several domains: separate with spaces. Unlike the other settings,
                    this applies immediately.
                  </span>
                </div>
                <div class="akd-field">
                  <label class="akd-field__label" for="md-model">Hugging Face model</label>
                  <input
                    id="md-model"
                    name="modelId"
                    class="akd-input akd-input--mono"
                    required
                    [(ngModel)]="modelId"
                    [disabled]="busy()"
                  />
                </div>
                <div class="row">
                  <div class="akd-field">
                    <label class="akd-field__label" for="md-quant">Quantization</label>
                    <input
                      id="md-quant"
                      name="quantization"
                      class="akd-input akd-input--mono"
                      placeholder="engine default"
                      [(ngModel)]="quantization"
                      [disabled]="busy()"
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="md-maxlen">Max model len</label>
                    <input
                      id="md-maxlen"
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
                    <label class="akd-field__label" for="md-mem">Memory fraction</label>
                    <input
                      id="md-mem"
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
                    <label class="akd-field__label" for="md-tp">Tensor parallel</label>
                    <input
                      id="md-tp"
                      name="tensorParallel"
                      class="akd-input"
                      type="number"
                      min="1"
                      [(ngModel)]="tensorParallel"
                      [disabled]="busy()"
                    />
                  </div>
                </div>
                <div class="row">
                  <div class="akd-field grow">
                    <label class="akd-field__label" for="md-image">Image override</label>
                    <input
                      id="md-image"
                      name="image"
                      class="akd-input akd-input--mono"
                      [placeholder]="
                        mo.modality === 'omni'
                          ? 'required — an omni runtime has no image to default to'
                          : 'per-engine default for the server architecture'
                      "
                      [(ngModel)]="image"
                      [disabled]="busy()"
                    />
                  </div>
                  <div class="akd-field">
                    <label class="akd-field__label" for="md-shm">Shm size (MB)</label>
                    <input
                      id="md-shm"
                      name="shm"
                      class="akd-input"
                      type="number"
                      min="1"
                      placeholder="host IPC"
                      [(ngModel)]="shmSizeMb"
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
                </div>
                <div>
                  <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
                    Save settings
                  </button>
                </div>
              </form>
            </akd-card>
          }
          @case ('logs') {
            <akd-card title="Logs">
              <p class="akd-muted intro">
                The engine container's console — where the weight download and the startup narrate
                themselves. Follow refreshes every few seconds while a start runs.
              </p>
              <div class="row">
                <button
                  class="akd-btn akd-btn--secondary"
                  type="button"
                  (click)="loadLogs()"
                  [disabled]="busy()"
                >
                  {{ logs() === null ? 'Load logs' : 'Refresh' }}
                </button>
                <label class="akd-check">
                  <input
                    type="checkbox"
                    name="followLogs"
                    [ngModel]="follow()"
                    (ngModelChange)="setFollow($event)"
                  />
                  Follow
                </label>
              </div>
              @if (logsError(); as message) {
                <p class="akd-muted">{{ message }}</p>
              } @else if (logs(); as lines) {
                <div class="akd-log logs" tabindex="0" aria-label="Model logs">
                  @for (line of lines; track line.sequence) {
                    <div class="akd-log__line">
                      <span class="akd-log__msg">{{ line.message }}</span>
                    </div>
                  }
                  @if (lines.length === 0) {
                    <div class="akd-log__line">
                      <span class="akd-log__msg">— no output yet —</span>
                    </div>
                  }
                </div>
              }
            </akd-card>
          }
          @case ('envs') {
            <akd-card title="Environment variables">
              <p class="akd-muted intro">
                The same variable machinery as every resource — shared {{ '{' }}{{ '{' }}scope.KEY{{
                  '}'
                }}{{ '}' }}
                references resolve, server variables inherit. Your variable wins over anything
                managed, HF_TOKEN included. Changes apply at the next start.
              </p>
              @for (env of envs(); track env.uuid) {
                <div class="row">
                  <code class="akd-mono">{{ env.key }}</code>
                  <span class="akd-mono akd-muted envval">{{
                    env.is_redacted ? '••••••' : (env.value ?? '')
                  }}</span>
                  <span class="grow"></span>
                  <button
                    class="akd-iconbtn"
                    type="button"
                    aria-label="Delete variable"
                    (click)="deleteEnv(env.uuid ?? '')"
                    [disabled]="busy()"
                  >
                    <akd-icon name="x" [size]="14" />
                  </button>
                </div>
              }
              <form class="row" (ngSubmit)="addEnv()">
                <input
                  name="envKey"
                  class="akd-input akd-input--mono"
                  placeholder="KEY"
                  [(ngModel)]="envKey"
                  [disabled]="busy()"
                />
                <input
                  name="envValue"
                  class="akd-input akd-input--mono"
                  placeholder="value"
                  [(ngModel)]="envValue"
                  [disabled]="busy()"
                />
                <label class="akd-check">
                  <input
                    type="checkbox"
                    name="envSecret"
                    [(ngModel)]="envSecret"
                    [disabled]="busy()"
                  />
                  Secret
                </label>
                <button
                  class="akd-btn akd-btn--secondary"
                  type="submit"
                  [disabled]="busy() || !envKey.trim()"
                >
                  Add
                </button>
              </form>
            </akd-card>
          }
        }
      } @else if (error(); as message) {
        <p class="akd-error" role="alert">{{ message }}</p>
      } @else {
        <p class="akd-muted">Loading…</p>
      }
    </div>
  `,
  styles: [
    `
      .grow {
        flex: 1;
      }
      .row {
        display: flex;
        gap: var(--space-3);
        align-items: center;
        margin-bottom: var(--space-2);
      }
      .row .akd-field {
        flex: 1;
      }
      .command {
        white-space: pre-wrap;
        word-break: break-all;
      }
      .jobbanner {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-2) var(--space-3);
        margin-bottom: var(--space-4);
        border: 1px solid var(--border);
        border-radius: var(--radius-2, 6px);
      }
      .logs {
        max-height: 24rem;
        overflow: auto;
      }
      .envval {
        max-width: 24rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      akd-card {
        display: block;
        margin-bottom: var(--space-5);
      }
    `,
  ],
})
export class ModelDetailComponent {
  private readonly api = inject(ApiService);
  private readonly router = inject(Router);
  private readonly confirm = inject(ConfirmService);
  private readonly route = inject(ActivatedRoute);
  private readonly uuid = this.route.snapshot.paramMap.get('uuid') ?? '';

  /**
   * Each tab names the permission it is useless without (the app pages'
   * convention): a reviewer keeps Overview, the server refuses the rest anyway.
   */
  private readonly allTabs: readonly { id: TabId; label: string; permission?: string }[] = [
    { id: 'overview', label: 'Overview' },
    { id: 'settings', label: 'Settings', permission: 'models:update' },
    { id: 'logs', label: 'Logs', permission: 'logs:read' },
    { id: 'envs', label: 'Environment variables', permission: 'secrets:read' },
  ];
  protected readonly tabs = computed(() =>
    this.allTabs.filter((t) => !t.permission || this.api.can(t.permission)),
  );
  protected readonly tab = signal<TabId>('overview');
  /** The active tab lives in the URL (?tab=…): a refresh keeps it, and
   * back/forward walk the tabs. */
  readonly tabParam = input<string | undefined>(undefined, { alias: 'tab' });

  protected readonly model = signal<Model | null>(null);
  protected readonly command = signal<string | null>(null);
  protected readonly commandMasked = signal(true);
  protected readonly apiKey = signal<string | null>(null);
  protected readonly flags = signal<EngineFlag[]>([]);
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected readonly version = computed(() => this.model()?.version ?? 0);

  protected modelId = '';
  protected quantization = '';
  protected maxModelLen: number | null = null;
  protected memoryFraction: number | null = null;
  protected tensorParallel = 1;
  protected image = '';
  protected shmSizeMb: number | null = null;
  protected domains = '';
  private savedDomains = '';

  // Runtime console + variables + the active-job poll (this tranche's UX).
  protected readonly logs = signal<LogLine[] | null>(null);
  protected readonly logsError = signal<string | null>(null);
  protected readonly follow = signal(false);
  protected readonly envs = signal<EnvVar[]>([]);
  protected envKey = '';
  protected envValue = '';
  protected envSecret = false;

  private followTimer: ReturnType<typeof setInterval> | null = null;
  private jobTimer: ReturnType<typeof setInterval> | null = null;

  protected readonly actions = computed<ActionItem[]>(() => [
    {
      id: 'start',
      label: 'Start',
      icon: 'play',
      hint: 'Recreate the container from the current configuration and load the weights',
    },
    {
      id: 'stop',
      label: 'Stop',
      icon: 'square',
      hint: 'Free the GPU memory — the endpoint pauses, the weights stay cached',
    },
    {
      id: 'restart',
      label: 'Restart',
      icon: 'rotate-cw',
      hint: 'Stop then start, same configuration',
    },
    {
      id: 'delete',
      label: 'Delete',
      icon: 'trash-2',
      danger: true,
      hint: 'Remove the model — the shared weights cache is kept',
    },
  ]);

  constructor() {
    void this.load();
    void this.loadEnvs();
    // URL -> state: seeds the tab on load and follows back/forward.
    effect(() => {
      const wanted = this.tabParam();
      const valid = this.tabs().find((t) => t.id === wanted)?.id;
      this.tab.set(valid ?? 'overview');
    });
    const destroyRef = inject(DestroyRef);
    destroyRef.onDestroy(() => {
      if (this.followTimer) clearInterval(this.followTimer);
      if (this.jobTimer) clearInterval(this.jobTimer);
    });
    // While a lifecycle job is queued or running, the page keeps itself
    // honest: poll the model until the job reaches a terminal state.
    effect(() => {
      const active = !!this.model()?.active_job;
      untracked(() => {
        if (active && this.jobTimer === null) {
          this.jobTimer = setInterval(() => void this.refreshStatus(), 4000);
        } else if (!active && this.jobTimer !== null) {
          clearInterval(this.jobTimer);
          this.jobTimer = null;
        }
      });
    });
  }

  protected selectTab(id: TabId): void {
    if (this.tab() === id) return;
    void this.router.navigate([], {
      relativeTo: this.route,
      queryParams: { tab: id === 'overview' ? null : id },
      queryParamsHandling: 'merge',
    });
  }

  protected runAction(action: string): void {
    switch (action) {
      case 'start':
        void this.start();
        break;
      case 'stop':
        void this.stop();
        break;
      case 'restart':
        void this.restart();
        break;
      case 'delete':
        void this.remove();
        break;
    }
  }

  /** Status-only refresh: never resyncs the settings form under an edit. */
  private async refreshStatus(): Promise<void> {
    try {
      this.model.set(await this.api.client().getModel(this.uuid));
    } catch {
      /* transient — the next tick retries */
    }
  }

  protected async cancelActiveJob(jobUuid: string): Promise<void> {
    this.busy.set(true);
    try {
      // A job that had not started is already gone; one in flight has only
      // been asked, and saying "cancelled" there would promise a GPU that is
      // not free yet.
      const job = await this.api.client().cancelJob(jobUuid);
      this.notice.set(
        job.cancel_requested_at
          ? 'Cancellation requested — the job stops at its next checkpoint.'
          : 'Job cancelled.',
      );
      await this.refreshStatus();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async loadLogs(): Promise<void> {
    this.logsError.set(null);
    try {
      const res = await this.api.client().getModelLogs(this.uuid, { lines: 400 });
      this.logs.set(res.data);
    } catch (err) {
      this.logsError.set(ApiService.describe(err));
      this.setFollow(false);
    }
  }

  protected setFollow(on: boolean): void {
    this.follow.set(on);
    if (on && this.followTimer === null) {
      void this.loadLogs();
      this.followTimer = setInterval(() => void this.loadLogs(), 3000);
    } else if (!on && this.followTimer !== null) {
      clearInterval(this.followTimer);
      this.followTimer = null;
    }
  }

  private async loadEnvs(): Promise<void> {
    try {
      const res = await this.api.client().listModelEnvs(this.uuid, { limit: 100 });
      this.envs.set(res.data);
    } catch {
      /* the card simply shows no variables */
    }
  }

  protected async addEnv(): Promise<void> {
    if (!this.envKey.trim()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().createModelEnv(this.uuid, {
        key: this.envKey.trim(),
        value: this.envValue,
        is_secret: this.envSecret,
        is_build_time: false,
        is_literal: false,
        is_multiline: false,
        is_locked: false,
      });
      this.envKey = '';
      this.envValue = '';
      this.envSecret = false;
      this.notice.set('Variable added — it reaches the engine at the next start.');
      await this.loadEnvs();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteEnv(envUuid: string): Promise<void> {
    if (!envUuid) return;
    this.busy.set(true);
    try {
      await this.api.client().deleteModelEnv(this.uuid, envUuid);
      await this.loadEnvs();
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async load(): Promise<void> {
    try {
      const model = await this.api.client().getModel(this.uuid);
      this.setModel(model);
      await this.loadCommand(false);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  /** The copy-ready public base URLs, one per domain. */
  protected publicUrls(): string[] {
    return (this.model()?.domains ?? []).map((d) =>
      d.includes('/') ? `https://${d}` : `https://${d}/v1`,
    );
  }

  private setModel(model: Model): void {
    this.model.set(model);
    this.modelId = model.model_id;
    this.quantization = model.quantization ?? '';
    this.maxModelLen = model.max_model_len ?? null;
    this.memoryFraction = model.memory_fraction ?? null;
    this.tensorParallel = model.tensor_parallel_size ?? 1;
    this.image = model.image ?? '';
    this.shmSizeMb = model.shm_size_mb ?? null;
    this.domains = (model.domains ?? []).join(' ');
    this.savedDomains = this.domains;
    this.flags.set(model.engine_flags ?? []);
  }

  protected async loadCommand(reveal: boolean): Promise<void> {
    try {
      const res = await this.api
        .client()
        .getModelCommand(this.uuid, reveal ? { reveal } : undefined);
      this.command.set(res.command);
      this.commandMasked.set(res.masked);
    } catch (err) {
      // A masked command everyone may read failed for a real reason; a
      // refused reveal simply keeps the mask.
      if (reveal) {
        this.error.set(ApiService.describe(err));
      }
    }
  }

  protected async revealKey(): Promise<void> {
    this.error.set(null);
    try {
      const creds = await this.api.client().getModelCredentials(this.uuid);
      this.apiKey.set(creds.api_key);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    }
  }

  protected async copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      this.notice.set('Copied.');
    } catch {
      this.notice.set('Copy failed — select and copy by hand.');
    }
  }

  /**
   * The soft occupied-GPU guard (ADR-080 §5): a 409 names the running
   * neighbour, and confirming IS the swap — one job stops it and starts
   * this one, in order.
   */
  protected async start(): Promise<void> {
    this.error.set(null);
    this.busy.set(true);
    try {
      await this.api.client().startModel(this.uuid);
      this.notice.set('Start accepted — weights load in the background (minutes on first run).');
      await this.refreshStatus();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409 && err.code === 'operation_in_progress') {
        this.error.set(err.message);
        await this.refreshStatus();
        return;
      }
      if (err instanceof ApiError && err.status === 409 && err.code === 'gpu_busy') {
        this.busy.set(false);
        // Two ways forward, not one: the declared fractions do not fit, but
        // they are a declaration — the operator reading the card may know
        // better (ADR-082 §3). The details carry the arithmetic.
        const choice = await this.confirm.askChoice({
          title: 'The GPU does not have room',
          message: err.message,
          bullets: err.details.map((d) => d.message),
          confirmLabel: 'Stop them and start this one',
          alternativeLabel: 'Start it alongside',
          danger: false,
        });
        if (choice === 'confirm') {
          await this.lifecycle(
            () => this.api.client().startModel(this.uuid, { swap: true }),
            'Swap accepted — the running models stop first, then this one loads.',
          );
        } else if (choice === 'alternative') {
          await this.lifecycle(
            () => this.api.client().startModel(this.uuid, { force: true }),
            'Start accepted alongside the running models — watch the logs for an out-of-memory failure.',
          );
        }
        return;
      }
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async stop(): Promise<void> {
    await this.lifecycle(
      () => this.api.client().stopModel(this.uuid),
      'Stop accepted — the endpoint pauses; the weights stay cached for the resume.',
    );
  }

  protected async restart(): Promise<void> {
    await this.lifecycle(() => this.api.client().restartModel(this.uuid), 'Restart accepted.');
  }

  private async lifecycle(action: () => Promise<unknown>, message: string): Promise<void> {
    this.error.set(null);
    this.busy.set(true);
    try {
      await action();
      this.notice.set(message);
      await this.refreshStatus();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409 && err.code === 'operation_in_progress') {
        this.error.set(err.message);
        await this.refreshStatus();
        return;
      }
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    const model = this.model();
    if (!model) return;
    if (
      !(await this.confirm.ask({
        title: 'Delete the model',
        message: `Delete ${model.name}? The container is removed; the shared weights cache on the server is kept.`,
        confirmLabel: 'Delete',
      }))
    ) {
      return;
    }
    this.busy.set(true);
    try {
      await this.api.client().deleteModel(this.uuid);
      void this.router.navigate(['/models']);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.busy.set(false);
    }
  }

  protected addFlag(): void {
    this.flags.set([...this.flags(), { flag: '' }]);
  }

  protected removeFlag(index: number): void {
    this.flags.set(this.flags().filter((_, i) => i !== index));
  }

  protected setFlag(index: number, key: 'flag' | 'value', value: string): void {
    this.flags.set(this.flags().map((flag, i) => (i === index ? { ...flag, [key]: value } : flag)));
  }

  protected async save(): Promise<void> {
    const model = this.model();
    if (!model || this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    this.notice.set(null);
    try {
      const updated = await this.api.client().updateModel(this.uuid, this.version(), {
        model_id: this.modelId.trim(),
        quantization: this.quantization.trim() || null,
        max_model_len: this.maxModelLen,
        memory_fraction: this.memoryFraction,
        tensor_parallel_size: this.tensorParallel,
        image: this.image.trim() || null,
        shm_size_mb: this.shmSizeMb,
        ...(this.domains.trim() === this.savedDomains.trim()
          ? {}
          : { domains: splitDomains(this.domains) }),
        engine_flags: this.flags()
          .filter((flag) => flag.flag.trim() !== '')
          .map((flag) => ({
            flag: flag.flag.trim(),
            ...(flag.value?.trim() ? { value: flag.value.trim() } : {}),
          })),
      });
      this.setModel(updated);
      await this.loadCommand(false);
      this.notice.set('Settings saved — they apply at the next start.');
    } catch (err) {
      if (err instanceof ApiError && err.isVersionConflict) {
        await this.load();
        this.error.set(
          'Your edit raced a concurrent change: the latest configuration was reloaded. Re-apply your edit on top of it.',
        );
      } else {
        this.error.set(ApiService.describe(err));
      }
    } finally {
      this.busy.set(false);
    }
  }
}
