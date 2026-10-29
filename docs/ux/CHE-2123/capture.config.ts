import config from "../../../apps/desktop/electron.vite.config";

export default {
  ...config,
  main: {
    ...config.main,
    plugins: [
      ...(config.main?.plugins ?? []),
      {
        name: "che-2123-isolated-capture",
        enforce: "pre",
        transform(_code: string, id: string) {
          if (id.endsWith("/daemon-manager.ts")) return "export function setupDaemonManager() {}";
          if (id.endsWith("/updater.ts")) return "export function setupAutoUpdater() {}";
        },
      },
    ],
  },
};
