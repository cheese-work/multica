import { defineConfig } from "vitest/config";
import path from "node:path";

// Mobile vitest is intentionally minimal — Node environment only, scoped to
// pure-function helpers in `lib/` and headless data-layer logic in `data/`
// (query-key + cache-patch functions that take a QueryClient but touch no
// DOM / RN native modules). We don't ship jsdom or RN test renderers here
// because the app runs on Hermes / native shims and any DOM-shaped runner
// would be a lie. Tests that need RN component rendering would need a
// separate jest+react-native-testing-library track; for now we keep this
// lane for helpers, serializers, and cache updaters only. Data-layer tests
// must mock `@/data/api` so the native fetch chain never loads.
//
// Co-located test files (foo.ts + foo.test.ts) match how the rest of the
// monorepo organises vitest suites.
// The composer draft test renders the real composers with react-test-renderer
// and every native module mocked: it checks that a draft survives unmount and
// remount, not native rendering.
// The run cancel button has headless callback/prop tests with native modules
// mocked; these do not verify native rendering.
// The form-draft tests (`data/stores/use-form-drafts.test.ts` and
// `*-screen.test.tsx`) opt into jsdom per file. The screen tests render the real
// screens with react-native swapped for DOM stubs (`data/stores/test-support/`);
// they cover wiring and state, not native rendering.
// App configuration tests also run here to cover native prebuild options
// across environment variants without loading React Native.
export default defineConfig({
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "."),
    },
  },
  test: {
    environment: "node",
    globals: true,
    include: [
      "app.config.test.ts",
      "plugins/**/*.test.ts",
      "lib/**/*.test.ts",
      "data/**/*.test.ts",
      "data/**/*.test.tsx",
      "components/issue/run-cancel-button.test.ts",
      "components/composer/composer-draft.test.tsx",
      "components/issue/issue-detail-load-error.test.tsx",
    ],
    passWithNoTests: true,
  },
});
