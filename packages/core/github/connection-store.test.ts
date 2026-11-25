import { beforeEach, expect, it } from "vitest";
import { useGitHubConnectionStore } from "./connection-store";

const event = (id?: string, workspace = "ws-1") => ({
  installation: { id: "install-1", workspace_id: workspace },
  ...(id ? { connection_id: id } : {}),
});
const installationID = () => useGitHubConnectionStore.getState().connection?.installationID;

beforeEach(() => useGitHubConnectionStore.getState().reset());

it("completes only the flow whose connection id it began", () => {
  const store = useGitHubConnectionStore.getState();
  store.begin("ws-1", "desktop-a");
  store.complete(event("desktop-b"));
  expect(installationID()).toBe("");
  store.complete(event("desktop-a"));
  expect(installationID()).toBe("install-1");
});

it("ignores an installation event that names no flow", () => {
  const store = useGitHubConnectionStore.getState();
  store.begin("ws-1", "desktop-a");
  store.complete(event());
  expect(installationID()).toBe("");
});

it("never completes a pending flow that has no connection id", () => {
  const store = useGitHubConnectionStore.getState();
  store.begin("ws-1", "");
  store.complete(event("desktop-a"));
  store.complete(event());
  expect(installationID()).toBe("");
});
