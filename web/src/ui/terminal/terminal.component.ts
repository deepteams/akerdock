import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  ElementRef,
  computed,
  inject,
  input,
  signal,
  viewChild,
} from '@angular/core';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { ApiService } from '../../app/core/api.service';
import {
  attachUrl,
  describeEnd,
  parseEndMessage,
  resizeMessage,
  type Geometry,
  type TerminalSessionInfo,
} from './protocol';

/** How the host page opens a session — the caller owns the API call. */
export type OpenSession = () => Promise<TerminalSessionInfo>;

/**
 * Web terminal (§5.7, §24.4, ADR-024): xterm.js over a WebSocket bridged to a
 * remote PTY.
 *
 * Nothing is connected until the operator asks: a terminal that opens itself
 * on tab switch would open a session — and an audit entry — for someone who
 * only wanted to look at the page.
 */
@Component({
  selector: 'akd-terminal',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="akd-bar">
      <div>
        <h2>{{ title() }}</h2>
        @if (state() === 'open') {
          <p class="akd-muted">
            Idle timeout {{ minutes(idleTimeout()) }} · maximum {{ minutes(maxDuration()) }}.
            Keystrokes are not recorded; opening and closing are audited.
          </p>
        } @else {
          <p class="akd-muted">{{ hint() }}</p>
        }
      </div>
      <div class="actions">
        @if (state() === 'open') {
          <button class="akd-btn-danger" type="button" (click)="disconnect()">Close session</button>
        } @else {
          <button
            class="akd-btn"
            type="button"
            [disabled]="state() === 'opening'"
            (click)="connect()"
          >
            {{ state() === 'opening' ? 'Connecting…' : 'Open terminal' }}
          </button>
        }
      </div>
    </div>

    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }
    @if (notice(); as message) {
      <p class="akd-muted" role="status">{{ message }}</p>
    }

    <!-- The host stays in the DOM across connects: xterm attaches once, and a
         closed session keeps its scrollback on screen (§5.7). -->
    <div class="screen" [class.idle]="state() !== 'open'" #screen></div>
  `,
  styles: [
    `
      :host {
        display: block;
      }
      h2 {
        margin: 0;
        font-size: var(--akd-text-sm);
        font-weight: var(--akd-weight-semibold);
        color: var(--akd-text);
      }
      .actions {
        display: flex;
        gap: var(--akd-space-2);
      }
      .screen {
        margin-top: var(--akd-space-3);
        padding: var(--akd-space-2);
        /* A FIXED height, never content-driven: the fit addon sizes the rows
           from this box, and the box must not size itself from the rows — a
           min-height alone lets xterm grow the box, the observer refit more
           rows, and the page stretch forever. */
        height: clamp(20rem, 60vh, 40rem);
        overflow: hidden;
        border: 1px solid var(--akd-border);
        border-radius: var(--akd-radius-lg);
        /* The log/terminal surface is dark in both themes (design-system §2.6). */
        background: var(--akd-log-bg);
      }
      .screen.idle {
        opacity: 0.7;
      }
    `,
  ],
})
export class TerminalComponent {
  /** What the header says — "Application shell", "Server shell"… */
  readonly title = input<string>('Terminal');
  /** Shown before connecting: name the blast radius when it deserves it. */
  readonly hint = input<string>('');
  /** Opens the session. The caller decides which endpoint that is. */
  readonly open = input.required<OpenSession>();

  private readonly api = inject(ApiService);
  private readonly screen = viewChild.required<ElementRef<HTMLDivElement>>('screen');

  protected readonly state = signal<'idle' | 'opening' | 'open'>('idle');
  protected readonly error = signal<string | null>(null);
  protected readonly notice = signal<string | null>(null);
  private readonly session = signal<TerminalSessionInfo | null>(null);
  protected readonly idleTimeout = computed(() => this.session()?.idle_timeout_seconds ?? 0);
  protected readonly maxDuration = computed(() => this.session()?.max_duration_seconds ?? 0);

  private term?: Terminal;
  private fit?: FitAddon;
  private socket?: WebSocket;
  private resizeObserver?: ResizeObserver;
  private encoder = new TextEncoder();

  constructor() {
    inject(DestroyRef).onDestroy(() => this.teardown());
  }

  protected minutes(seconds: number): string {
    if (seconds >= 3600) {
      const hours = Math.round(seconds / 360) / 10;
      return `${hours}h`;
    }
    return `${Math.round(seconds / 60)} min`;
  }

  protected async connect(): Promise<void> {
    this.error.set(null);
    this.notice.set(null);
    this.state.set('opening');
    try {
      const session = await this.open()();
      this.session.set(session);
      this.attach(session);
    } catch (err) {
      this.state.set('idle');
      this.error.set(ApiService.describe(err));
    }
  }

  protected disconnect(): void {
    // A clean close tells the server the user meant it (user_close), rather
    // than leaving it to infer a dropped connection.
    this.socket?.close(1000, 'user_close');
  }

  private attach(session: TerminalSessionInfo): void {
    const term = this.ensureTerminal();
    term.reset();
    this.fit?.fit();

    const geometry: Geometry = { cols: term.cols, rows: term.rows };
    const socket = new WebSocket(attachUrl(session, globalThis.location.origin, geometry));
    socket.binaryType = 'arraybuffer';
    this.socket = socket;

    socket.onopen = () => {
      this.state.set('open');
      term.focus();
    };
    socket.onmessage = (event) => {
      if (typeof event.data === 'string') {
        const reason = parseEndMessage(event.data);
        if (reason) this.notice.set(describeEnd(reason));
        return;
      }
      term.write(new Uint8Array(event.data as ArrayBuffer));
    };
    socket.onerror = () => {
      if (this.state() === 'opening') this.error.set('The terminal connection failed to open.');
    };
    socket.onclose = () => {
      this.state.set('idle');
      this.socket = undefined;
      this.session.set(null);
      if (!this.notice() && !this.error()) this.notice.set('Session closed.');
    };

    // Keystrokes out. Binary frames: a terminal carries bytes, not text — a
    // paste of invalid UTF-8 must reach the shell as typed.
    term.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(this.encoder.encode(data));
    });
    term.onResize(({ cols, rows }) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(resizeMessage({ cols, rows }));
    });
  }

  /** Creates the xterm instance on first connect and reuses it afterwards. */
  private ensureTerminal(): Terminal {
    if (this.term) return this.term;
    const term = new Terminal({
      convertEol: false,
      cursorBlink: true,
      fontFamily: getComputedStyle(document.documentElement).getPropertyValue('--akd-font-mono'),
      fontSize: 13,
      scrollback: 5000,
      theme: {
        background: cssValue('--akd-log-bg'),
        foreground: cssValue('--akd-log-fg'),
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(this.screen().nativeElement);
    fit.fit();

    // The pty must follow the window: a shell drawing at 80 columns inside a
    // 200-column pane is not a cosmetic problem, it is a corrupted display.
    this.resizeObserver = new ResizeObserver(() => fit.fit());
    this.resizeObserver.observe(this.screen().nativeElement);

    this.term = term;
    this.fit = fit;
    return term;
  }

  private teardown(): void {
    this.resizeObserver?.disconnect();
    this.socket?.close(1000, 'user_close');
    this.term?.dispose();
  }
}

function cssValue(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
