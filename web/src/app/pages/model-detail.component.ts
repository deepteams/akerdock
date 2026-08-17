import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { StatusBadgeComponent } from '../../ui/status-badge/status-badge.component';
import { ConfirmService } from '../../ui/confirm/confirm.service';
import { ApiError } from '../../api/client';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Model = components['schemas']['Model'];
type EngineFlag = components['schemas']['EngineFlag'];

// One model (ADR-080): lifecycle with the soft occupied-GPU guard — a 409
// names the running neighbour and the confirm IS the swap —, the serve
// command by the deployment's own renderer (masked, revealed under
// models:credentials), the managed key, and the typed settings.
@Component({
  selector: 'app-model-detail',
  standalone: true,
  imports: [FormsModule, RouterLink, CardComponent, IconComponent, StatusBadgeComponent],
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
          <akd-status-badge domain="resource" [state]="mo.status" />
          <akd-status-badge domain="resource" [state]="mo.observed_status ?? 'unknown'" />
          <span class="grow"></span>
          <button
            class="akd-btn akd-btn--primary"
            type="button"
            (click)="start()"
            [disabled]="busy()"
          >
            Start
          </button>
          <button class="akd-btn akd-btn--secondary" type="button" (click)="stop()" [disabled]="busy()">
            Stop
          </button>
          <button
            class="akd-btn akd-btn--secondary"
            type="button"
            (click)="restart()"
            [disabled]="busy()"
          >
            Restart
          </button>
          <button
            class="akd-btn akd-btn--danger"
            type="button"
            (click)="remove()"
            [disabled]="busy()"
          >
            Delete
          </button>
        </header>

        @if (error(); as message) {
          <p class="akd-error" role="alert">{{ message }}</p>
        }
        @if (notice(); as message) {
          <p class="akd-muted" role="status">{{ message }}</p>
        }

        <akd-card title="Endpoint">
          <p class="akd-muted intro">
            OpenAI-compatible, on the server's LAN address — protected by the managed API key
            the engine itself enforces. A stopped model keeps its endpoint and its key; the
            weights stay in the server's shared cache, so a resume reloads without
            re-downloading.
          </p>
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

        <akd-card title="Settings">
          <p class="akd-muted intro">
            Engine, server and port are immutable. Changes apply at the next start — the serve
            flags are read once, when the process starts.
          </p>
          <form class="form" (ngSubmit)="save()">
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
                  placeholder="per-engine default for this server's CPU"
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
  private readonly uuid = inject(ActivatedRoute).snapshot.paramMap.get('uuid') ?? '';

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

  constructor() {
    void this.load();
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

  private setModel(model: Model): void {
    this.model.set(model);
    this.modelId = model.model_id;
    this.quantization = model.quantization ?? '';
    this.maxModelLen = model.max_model_len ?? null;
    this.memoryFraction = model.memory_fraction ?? null;
    this.tensorParallel = model.tensor_parallel_size ?? 1;
    this.image = model.image ?? '';
    this.shmSizeMb = model.shm_size_mb ?? null;
    this.flags.set(model.engine_flags ?? []);
  }

  protected async loadCommand(reveal: boolean): Promise<void> {
    try {
      const res = await this.api.client().getModelCommand(this.uuid, reveal ? { reveal } : undefined);
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
    } catch (err) {
      if (err instanceof ApiError && err.status === 409 && err.code === 'gpu_busy') {
        this.busy.set(false);
        const swap = await this.confirm.ask({
          title: 'The GPU is occupied',
          message: err.message + ' Stop it and start this model instead?',
          confirmLabel: 'Stop it and start this one',
        });
        if (swap) {
          await this.lifecycle(() => this.api.client().startModel(this.uuid, { swap: true }),
            'Swap accepted — the running model stops first, then this one loads.');
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
    } catch (err) {
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
    this.flags.set(
      this.flags().map((flag, i) => (i === index ? { ...flag, [key]: value } : flag)),
    );
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
