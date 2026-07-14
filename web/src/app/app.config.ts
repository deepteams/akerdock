import {
  ApplicationConfig,
  provideBrowserGlobalErrorListeners,
  provideZoneChangeDetection,
} from '@angular/core';
import { provideRouter, withComponentInputBinding } from '@angular/router';

import { routes } from './app.routes';

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    provideZoneChangeDetection({ eventCoalescing: true }),
    // withComponentInputBinding: a route parameter arrives as a component input,
    // so a page reads :uuid as a typed signal instead of subscribing to the
    // router — the URL stays the single source of truth for what is displayed.
    provideRouter(routes, withComponentInputBinding()),
  ],
};
