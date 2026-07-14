// Storybook — the living catalogue the design system requires (§5.2): the
// spec document defines the tokens and rules, this catalogue is what "faith"
// means for the implementation.
import type { StorybookConfig } from '@storybook/angular';

const config: StorybookConfig = {
  stories: ['../src/**/*.stories.ts'],
  // addon-a11y runs axe-core on every story (§5.2: a11y violations must be
  // able to fail CI, not sit in a report nobody opens).
  addons: ['@storybook/addon-a11y'],
  framework: {
    name: '@storybook/angular',
    options: {},
  },
};

export default config;
