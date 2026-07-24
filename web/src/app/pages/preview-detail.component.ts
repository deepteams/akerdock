import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../ui/card/card.component';
import { IconComponent } from '../../ui/icon/icon.component';
import { ApplicationEnvsTabComponent } from './application/envs-tab.component';
import { ApiService } from '../core/api.service';
import type { components } from '../../api/schema';

type Preview = components['schemas']['Preview'];
type LogLine = components['schemas']['LogLine'];
type ServiceComponent = components['schemas']['ServiceComponent'];
type Storage = components['schemas']['PersistentStorage'];

/**
 * Everything of ONE PR instance in one place (§20.4): where it runs, its
 * containers' consoles, its derived volumes, the preview variable set, and
 * its own danger zone — because debugging a preview through production's
 * pages meant debugging blind.
 */
@Component({
  selector: 'app-preview-detail',
  standalone: true,
  imports: [
    FormsModule,
    RouterLink,
    CardComponent,
    IconComponent,
    ApplicationEnvsTabComponent,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="head">
      <a class="akd-btn akd-btn--ghost akd-btn--sm" [routerLink]="['/applications', uuid()]">
        <akd-icon name="arrow-left" [size]="14" />
        Application
      </a>
      @if (preview(); as p) {
        <h1 class="title">
          <span class="akd-badge akd-badge--mono">PR #{{ p.pr_id }}</span>
          {{ p.source_branch }}
        </h1>
        <span class="akd-badge">{{ p.status }}</span>
        @if (p.is_fork) {
          <span class="akd-badge akd-badge--accent">fork</span>
        }
        <span class="spacer"></span>
        @if (p.fqdn) {
          <a class="akd-btn akd-btn--secondary akd-btn--sm" [href]="'https://' + p.fqdn" target="_blank" rel="noopener">
            <akd-icon name="external-link" [size]="13" />
            {{ p.fqdn }}
          </a>
        }
      }
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    @if (preview(); as p) {
      <div class="stack">
        <akd-card title="Logs" [padded]="false">
          <div class="toolbar">
            @if (components().length > 0) {
              <div class="akd-select">
                <select name="component" class="akd-input" [(ngModel)]="component" (ngModelChange)="refreshLogs()">
                  @for (c of components(); track c.name) {
                    <option [ngValue]="c.name">{{ c.name }}</option>
                  }
                </select>
              </div>
            }
            <div class="akd-select">
              <select name="lines" class="akd-input" [(ngModel)]="lines" (ngModelChange)="refreshLogs()">
                <option [ngValue]="200">Last 200 lines</option>
                <option [ngValue]="500">Last 500 lines</option>
                <option [ngValue]="2000">Last 2000 lines</option>
              </select>
            </div>
            <label class="akd-check">
              <input type="checkbox" name="follow" [(ngModel)]="follow" (ngModelChange)="onFollow()" />
              Follow (refresh every 3 s)
            </label>
            <span class="spacer"></span>
            <button class="akd-btn akd-btn--secondary akd-btn--sm" type="button" [disabled]="busy()" (click)="refreshLogs()">
              <akd-icon name="refresh-cw" [size]="13" />
              Refresh
            </button>
          </div>
          @if (logs(); as logLines) {
            @if (logLines.length === 0) {
              <p class="akd-muted pad">The container has not written anything yet.</p>
            } @else {
              <pre class="log"><code>@for (line of logLines; track line.sequence) {{{ line.message }}
}</code></pre>
            }
          } @else if (busy()) {
            <p class="akd-muted pad">Loading…</p>
          }
        </akd-card>

        @if (storages().length > 0) {
          <akd-card title="Preview storages" [padded]="false">
            <p class="akd-muted pad">
              Derived from the application's storages — created empty (or cloned when the
              volume declares preview_seed), destroyed with the preview.
            </p>
            <table class="akd-table">
              <thead>
                <tr>
                  <th scope="col">Volume</th>
                  <th scope="col">Mount path</th>
                </tr>
              </thead>
              <tbody>
                @for (s of storages(); track s.uuid) {
                  <tr>
                    <td class="akd-mono">{{ previewVolumeName(s) }}</td>
                    <td class="akd-mono">{{ s.mount_path }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </akd-card>
        }

        <section>
          <h2 class="section-title">Environment variables · previews set</h2>
          <p class="akd-muted">
            This set is shared by every preview of the application (INV-010: production
            values are never inherited). Changes apply on the next preview deployment.
          </p>
          <app-application-envs-tab [uuid]="uuid()" />
        </section>

        <akd-card title="Danger zone">
          <div class="danger">
            <div>
              <strong>Destroy this preview</strong>
              <p class="akd-muted">
                Removes its containers, volumes, networks and routing. Production is never
                touched. The PR stays open — a /deploy comment or a push recreates a fresh
                instance.
              </p>
            </div>
            <button class="akd-btn akd-btn--danger" type="button" [disabled]="busy() || p.status === 'destroyed'" (click)="destroy()">
              Destroy preview
            </button>
          </div>
        </akd-card>
      </div>
    } @else if (!error()) {
      <p class="akd-muted">Loading…</p>
    }
  `,
  styles: [
    `
      .head {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        margin-bottom: var(--space-4);
      }
      .title {
        margin: 0;
        font-size: var(--text-lg);
        display: flex;
        align-items: center;
        gap: var(--space-2);
      }
      .spacer {
        flex: 1;
      }
      .stack {
        display: grid;
        gap: var(--space-5);
      }
      .section-title {
        margin: 0 0 var(--space-2);
        font-size: var(--text-md);
      }
      .toolbar {
        display: flex;
        align-items: center;
        gap: var(--space-3);
        padding: var(--space-3);
        border-bottom: 1px solid var(--border);
      }
      .pad {
        padding: var(--space-3);
      }
      .log {
        margin: 0;
        padding: var(--space-3);
        max-height: 60vh;
        overflow: auto;
        font-family: var(--font-mono);
        font-size: var(--text-xs);
        line-height: 1.5;
        white-space: pre-wrap;
        word-break: break-all;
        background: var(--log-bg, var(--surface-2));
      }
      .danger {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-4);
      }
    `,
  ],
})
export class PreviewDetailComponent {
  readonly uuid = input.required<string>();
  readonly previewUuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly router = inject(Router);

  protected readonly preview = signal<Preview | null>(null);
  protected readonly components = signal<ServiceComponent[]>([]);
  protected readonly storages = signal<Storage[]>([]);
  protected readonly logs = signal<LogLine[] | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected lines = 200;
  protected component = '';
  protected follow = false;
  private timer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    effect(() => {
      const app = this.uuid();
      const preview = this.previewUuid();
      untracked(() => void this.init(app, preview));
    });
    inject(DestroyRef).onDestroy(() => this.stopFollow());
  }

  protected previewVolumeName(s: Storage): string {
    return s.kind === 'volume' ? `${this.previewUuid()}_${s.name ?? ''}` : (s.host_path ?? '');
  }

  private async init(app: string, previewUuid: string): Promise<void> {
    try {
      const [previews, comps, storages] = await Promise.all([
        this.api.client().listApplicationPreviews(app),
        this.api.client().listApplicationComponents(app),
        this.api.client().listApplicationStorages(app),
      ]);
      const preview = previews.data.find((p) => p.uuid === previewUuid) ?? null;
      if (!preview) {
        this.error.set('Preview not found — it may have been removed.');
        return;
      }
      this.preview.set(preview);
      this.components.set(comps.data);
      this.storages.set(storages.data);
      if (comps.data.length > 0) this.component = comps.data[0].name;
    } catch (err) {
      this.error.set(ApiService.describe(err));
      return;
    }
    await this.loadLogs();
  }

  protected refreshLogs(): void {
    void this.loadLogs();
  }

  protected onFollow(): void {
    this.stopFollow();
    if (this.follow) {
      this.timer = setInterval(() => void this.loadLogs(), 3000);
    }
  }

  private stopFollow(): void {
    if (this.timer !== null) {
      clearInterval(this.timer);
      this.timer = null;
    }
  }

  private async loadLogs(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      const page = await this.api.client().getPreviewLogs(this.uuid(), this.previewUuid(), {
        lines: this.lines,
        ...(this.component ? { component: this.component } : {}),
      });
      this.logs.set(page.data);
      this.error.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
      this.follow = false;
      this.stopFollow();
    } finally {
      this.busy.set(false);
    }
  }

  protected async destroy(): Promise<void> {
    const p = this.preview();
    if (!p || !confirm(`Destroy the preview of PR #${p.pr_id}? Its containers and volumes will be removed.`)) {
      return;
    }
    this.busy.set(true);
    try {
      await this.api.client().destroyPreview(this.uuid(), this.previewUuid());
      await this.router.navigate(['/applications', this.uuid()]);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
