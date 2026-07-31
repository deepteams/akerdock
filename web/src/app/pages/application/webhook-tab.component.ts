import { ChangeDetectionStrategy, Component, inject, input, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { CardComponent } from '../../../ui/card/card.component';
import { ApiService } from '../../core/api.service';
import { ConfirmService } from '../../../ui/confirm/confirm.service';
import type { components } from '../../../api/schema';

type WebhookEndpoint = components['schemas']['WebhookEndpoint'];
type Provider = components['schemas']['WebhookEndpointCreate']['provider'];

@Component({
  selector: 'app-application-webhook-tab',
  standalone: true,
  imports: [FormsModule, CardComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (error(); as message) {
      <p class="akd-error" role="alert">{{ message }}</p>
    }

    <akd-card title="Webhook endpoint" class="create">
      <div class="body">
        <p class="akd-muted intro">
          Create an endpoint, declare its URL and secret in your Git provider, and pushes trigger
          deployments.
        </p>
        <form class="row" (ngSubmit)="create()">
          <div class="akd-field">
            <label class="akd-field__label" for="wh-provider">Provider</label>
            <div class="akd-select">
              <select
                id="wh-provider"
                name="provider"
                class="akd-input"
                [(ngModel)]="provider"
                [disabled]="busy()"
              >
                @for (p of providers; track p) {
                  <option [value]="p">{{ p }}</option>
                }
              </select>
            </div>
          </div>
          <button class="akd-btn akd-btn--primary" type="submit" [disabled]="busy()">
            Create endpoint
          </button>
          <button
            class="akd-btn akd-btn--danger"
            type="button"
            [disabled]="busy()"
            (click)="remove()"
          >
            Delete endpoint
          </button>
        </form>

        @if (created(); as endpoint) {
          <div>
            <h3 class="akd-field__label">URL to declare at {{ endpoint.provider }}</h3>
            <p class="akd-secret">{{ endpoint.url }}</p>
            <h3 class="akd-field__label">Signing secret</h3>
            <!-- The HMAC secret is returned once at creation and never again
                 (INV-003): copy it now or recreate the endpoint. -->
            <p class="akd-secret">{{ endpoint.secret }}</p>
            <p class="akd-muted" role="status">
              Copy the secret now — it is shown once and cannot be retrieved later.
            </p>
          </div>
        }
      </div>
    </akd-card>
  `,
  styles: [
    `
      .create {
        display: block;
        max-width: 44rem;
      }
      .body {
        display: grid;
        gap: var(--space-3);
      }
      .intro {
        margin: 0;
      }
      .row {
        display: flex;
        align-items: end;
        gap: var(--space-2);
        flex-wrap: wrap;
      }
      h3 {
        margin: var(--space-3) 0 var(--space-1);
      }
    `,
  ],
})
export class ApplicationWebhookTabComponent {
  readonly uuid = input.required<string>();

  private readonly api = inject(ApiService);
  private readonly confirm = inject(ConfirmService);

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
      !(await this.confirm.ask({
        title: 'Delete the webhook endpoint',
        message: `Delete the ${this.provider} webhook endpoint? Pushes from ${this.provider} will no longer trigger deployments.`,
        confirmLabel: 'Delete',
      }))
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
