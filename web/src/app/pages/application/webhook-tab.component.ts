import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../../core/api.service';
import type { components } from '../../../api/schema';

type WebhookEndpoint = components['schemas']['WebhookEndpoint'];
type Provider = components['schemas']['WebhookEndpointCreate']['provider'];

@Component({
  selector: 'app-application-webhook-tab',
  standalone: true,
  imports: [FormsModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <section class="akd-card create">
      <header class="akd-bar" style="margin-bottom: 0">
        <h2>Webhook endpoint</h2>
      </header>
      <p class="akd-muted">
        Create an endpoint, declare its URL and secret in your Git provider, and pushes trigger
        deployments.
      </p>
      <form class="row" (ngSubmit)="create()">
        <div class="akd-field">
          <label for="wh-provider">Provider</label>
          <select
            id="wh-provider"
            name="provider"
            class="akd-select"
            [(ngModel)]="provider"
            [disabled]="busy()"
          >
            @for (p of providers; track p) {
              <option [value]="p">{{ p }}</option>
            }
          </select>
        </div>
        <button class="akd-btn" type="submit" [disabled]="busy()">Create endpoint</button>
        <button class="akd-btn-danger" type="button" [disabled]="busy()" (click)="remove()">
          Delete endpoint
        </button>
      </form>

      @if (created(); as endpoint) {
        <div>
          <h3>URL to declare at {{ endpoint.provider }}</h3>
          <p class="akd-secret">{{ endpoint.url }}</p>
          <h3>Signing secret</h3>
          <!-- The HMAC secret is returned once at creation and never again
               (INV-003): copy it now or recreate the endpoint. -->
          <p class="akd-secret">{{ endpoint.secret }}</p>
          <p class="akd-muted" role="status">
            Copy the secret now — it is shown once and cannot be retrieved later.
          </p>
        </div>
      }
    </section>
  `,
  styles: [
    `
      .create {
        max-width: 44rem;
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--akd-space-2);
        flex-wrap: wrap;
      }
      h3 {
        margin: var(--akd-space-3) 0 var(--akd-space-1);
        font-size: var(--akd-text-sm);
        color: var(--akd-text);
      }
    `,
  ],
})
export class ApplicationWebhookTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);

  protected readonly providers: Provider[] = ['github', 'gitlab', 'gitea'];
  protected provider: Provider = 'github';

  protected readonly created = signal<WebhookEndpoint | null>(null);
  protected readonly error = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected async create(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set(null);
    try {
      const endpoint = await this.api
        .client()
        .createWebhookEndpoint(this.uuid(), { provider: this.provider });
      this.created.set(endpoint);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(): Promise<void> {
    if (
      !confirm(
        `Delete the ${this.provider} webhook endpoint? Pushes from ${this.provider} will no longer trigger deployments.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    try {
      await this.api.client().deleteWebhookEndpoint(this.uuid(), this.provider);
      if (this.created()?.provider === this.provider) this.created.set(null);
    } catch (err) {
      this.error.set(ApiService.describe(err));
    } finally {
      this.busy.set(false);
    }
  }
}
