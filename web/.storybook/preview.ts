// Global preview. The app's real stylesheet — tokens included — is injected
// by the Angular builder (`styles` option of the storybook targets), so a
// story renders with EXACTLY the variables production uses (§6.1: tokens
// only, no literal values). The theme toolbar flips the same `data-theme`
// attribute the dashboard sets.
import type { Preview } from '@storybook/angular';

const preview: Preview = {
  globalTypes: {
    theme: {
      description: 'Design-system theme (§2.6)',
      toolbar: {
        title: 'Theme',
        items: ['light', 'dark'],
        dynamicTitle: true,
      },
    },
  },
  initialGlobals: { theme: 'light' },
  decorators: [
    (story, context) => {
      document.documentElement.setAttribute('data-theme', context.globals['theme'] ?? 'light');
      return story();
    },
  ],
  parameters: {
    a11y: {
      // §5.2: a violation is a failure, not a warning in a panel.
      test: 'error',
    },
  },
};

export default preview;
