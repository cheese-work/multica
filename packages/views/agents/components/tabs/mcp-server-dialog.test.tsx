// @vitest-environment jsdom

import type { ComponentProps, ReactNode } from "react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useMcpDialogDraftStore } from "@multica/core/agents";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../../locales/en/common.json";
import enAgents from "../../../locales/en/agents.json";
import type { ManagedMcpServer } from "./mcp-config-model";
import { McpServerDialog } from "./mcp-server-dialog";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents } };

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function managedServer(overrides: Partial<ManagedMcpServer> = {}): ManagedMcpServer {
  return {
    name: "fetch",
    config: { command: "uvx" },
    container: "mcpServers",
    transport: "stdio",
    enabled: true,
    ...overrides,
  };
}

function renderDialog(overrides: Partial<ComponentProps<typeof McpServerDialog>> = {}) {
  const onSave = vi.fn().mockResolvedValue(undefined);
  const onOpenChange = vi.fn();
  const props = {
    open: true,
    server: null,
    existingNames: new Set<string>(),
    onSave,
    onOpenChange,
    ...overrides,
  };
  const view = render(<McpServerDialog {...props} />, { wrapper: Wrapper });
  return { ...view, onSave: props.onSave, onOpenChange: props.onOpenChange };
}

// Unmount first: a mounted draft hook would write its draft back after the reset.
afterEach(() => {
  cleanup();
  resetAllRegisteredDrafts();
});

describe("McpServerDialog", () => {
  it.each(["context7.dev", "legacy server", " padded "])(
    "edits configuration while preserving the historical name %s exactly",
    async (name) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderDialog({
        server: managedServer({ name }),
        existingNames: new Set([name]),
        hideNameWhenEditing: true,
      });

      expect(screen.queryByLabelText("Server name")).toBeNull();
      await user.clear(screen.getByLabelText("Command"));
      await user.type(screen.getByLabelText("Command"), "updated-command");
      await user.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() =>
        expect(onSave).toHaveBeenCalledWith(name, { command: "updated-command" }),
      );
      expect(onOpenChange).toHaveBeenCalledWith(false);
    },
  );

  it.each(["create", "rename"])(
    "rejects a newly entered historical-style name during %s",
    async (action) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderDialog({
        server: action === "rename" ? managedServer() : null,
      });

      const input = screen.getByLabelText("Server name");
      await user.clear(input);
      await user.type(input, "context7.dev");
      if (action === "create") {
        await user.type(screen.getByLabelText("Command"), "uvx");
      }
      await user.click(screen.getByRole("button", {
        name: action === "create" ? "Add" : "Save",
      }));

      expect(input).toHaveFocus();
      expect(input).toHaveAttribute("aria-invalid", "true");
      expect(screen.getByRole("alert")).toHaveTextContent(
        "Use only letters, numbers, hyphens, and underscores.",
      );
      expect(onSave).not.toHaveBeenCalled();
      expect(onOpenChange).not.toHaveBeenCalled();
    },
  );

  it.each([
    ["sse", "STDIO", "Command", "uvx", { command: "uvx" }],
    ["websocket", "Streamable HTTP", "Server URL", "https://new.example/mcp", {
      type: "http", url: "https://new.example/mcp",
    }],
  ])(
    "replaces a %s server through the visual editor using %s",
    async (transport, selected, label, value, expected) => {
      const user = userEvent.setup();
      const { onSave } = renderDialog({
        server: managedServer({ transport, config: {} }),
        replacementMode: true,
      });

      expect(screen.getByRole("tab", { name: "JSON" })).toHaveAttribute(
        "aria-selected", "true",
      );
      await user.click(screen.getByRole("tab", { name: "Visual editor" }));
      await user.click(screen.getByRole("button", { name: new RegExp(`^${selected}`) }));
      await user.type(screen.getByLabelText(label), value);
      await user.click(screen.getByRole("button", { name: "Replace configuration" }));

      expect(onSave).toHaveBeenCalledWith("fetch", expected);
    },
  );

  it("blocks duplicate submits and dismissal until a save completes", async () => {
    const user = userEvent.setup();
    let finishSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((resolve) => { finishSave = resolve; }));
    const { onOpenChange } = renderDialog({ server: managedServer(), onSave });

    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    fireEvent.submit(screen.getByLabelText("Command").closest("form")!);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByRole("dialog")).toBeVisible();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(onSave).toHaveBeenCalledExactlyOnceWith("fetch", { command: "uvx" });

    finishSave();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it.each(["Cancel", "Close", "Escape"])(
    "discards an unsaved draft when dismissed with %s",
    async (action) => {
      const user = userEvent.setup();
      const { onSave, onOpenChange } = renderDialog();
      await user.type(screen.getByLabelText("Server name"), "draft");

      if (action === "Escape") await user.keyboard("{Escape}");
      else await user.click(screen.getByRole("button", { name: action }));

      expect(onOpenChange).toHaveBeenCalledWith(false);
      expect(onSave).not.toHaveBeenCalled();
    },
  );

  it("clears submitted JSON errors when returning to the visual editor", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog();
    await user.type(screen.getByLabelText("Server name"), "fetch");
    await user.type(screen.getByLabelText("Command"), "uvx");
    await user.click(screen.getByRole("tab", { name: "JSON" }));
    fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), {
      target: { value: "{invalid" },
    });
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Invalid JSON");

    await user.click(screen.getByRole("tab", { name: "Visual editor" }));

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText("Command")).toHaveValue("uvx");
    expect(screen.getByLabelText("Command")).not.toHaveAttribute("aria-invalid");
    await user.click(screen.getByRole("button", { name: "Add" }));
    expect(onSave).toHaveBeenCalledWith("fetch", { command: "uvx" });
  });

  it("removes one argument and environment row while preserving their siblings", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({
      server: managedServer({
        config: {
          command: "uvx",
          args: ["first", "remove", "last"],
          env: { FIRST: "one", REMOVE: "two", LAST: "three" },
        },
      }),
    });

    await user.click(screen.getByRole("button", { name: "Remove argument 2" }));
    await user.clear(screen.getByLabelText("Startup arguments 2"));
    await user.type(screen.getByLabelText("Startup arguments 2"), "updated-last");
    await user.click(screen.getByRole("button", { name: "Remove environment variable 2" }));
    await user.clear(screen.getByLabelText("Environment variables: Variable name 2"));
    await user.type(screen.getByLabelText("Environment variables: Variable name 2"), "UPDATED");
    await user.clear(screen.getByLabelText("Environment variables: Value 2"));
    await user.type(screen.getByLabelText("Environment variables: Value 2"), "four");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave).toHaveBeenCalledWith("fetch", {
      command: "uvx",
      args: ["first", "updated-last"],
      env: { FIRST: "one", UPDATED: "four" },
    });
  });

  it("removes one HTTP header while preserving and editing its siblings", async () => {
    const user = userEvent.setup();
    const { onSave } = renderDialog({
      server: managedServer({
        transport: "http",
        config: {
          type: "http",
          url: "https://example.test/mcp",
          headers: { First: "one", Remove: "two", Last: "three" },
        },
      }),
    });

    await user.click(screen.getByRole("button", { name: "Remove header 2" }));
    await user.clear(screen.getByLabelText("HTTP headers: Header name 2"));
    await user.type(screen.getByLabelText("HTTP headers: Header name 2"), "Updated");
    await user.clear(screen.getByLabelText("HTTP headers: Value 2"));
    await user.type(screen.getByLabelText("HTTP headers: Value 2"), "four");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave).toHaveBeenCalledWith("fetch", {
      type: "http",
      url: "https://example.test/mcp",
      headers: { First: "one", Updated: "four" },
    });
  });

  it("restores a half-filled dialog after close and reopen, without env or header values", async () => {
    const user = userEvent.setup();
    const props = {
      server: null,
      existingNames: new Set<string>(),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:new",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    await user.type(screen.getByLabelText("Server name"), "half-done");
    await user.type(screen.getByLabelText("Command"), "uvx");

    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);

    expect(screen.getByLabelText("Server name")).toHaveValue("half-done");
    expect(screen.getByLabelText("Command")).toHaveValue("uvx");
  });

  it("clears the draft after a successful save", async () => {
    const user = userEvent.setup();
    const props = {
      server: null,
      existingNames: new Set<string>(),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:saved",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    await user.type(screen.getByLabelText("Server name"), "saved-one");
    await user.type(screen.getByLabelText("Command"), "uvx");
    await user.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(props.onSave).toHaveBeenCalled());

    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);
    expect(screen.getByLabelText("Server name")).toHaveValue("");
  });

  it("restores JSON-mode edits after reopen but never stores secret values", async () => {
    const user = userEvent.setup();
    const props = {
      server: null,
      existingNames: new Set<string>(),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:json",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    await user.click(screen.getByRole("tab", { name: "JSON" }));
    const field = screen.getByLabelText("MCP server JSON configuration");
    fireEvent.change(field, {
      target: {
        value: JSON.stringify({
          type: "sse",
          url: "https://example.test/sse",
          headers: { Authorization: "Bearer s3cret-token" },
        }),
      },
    });

    expect(JSON.stringify(useMcpDialogDraftStore.getState())).not.toContain("s3cret-token");
    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);

    const restored = JSON.parse(
      (screen.getByLabelText("MCP server JSON configuration") as HTMLTextAreaElement).value,
    );
    expect(restored).toEqual({ type: "sse", url: "https://example.test/sse" });
  });

  it("keeps the saved secret values of an edited server when restoring a JSON draft", async () => {
    const server = managedServer({
      name: "remote",
      config: { type: "sse", url: "https://a.test", headers: { Authorization: "saved" } },
      transport: "sse",
    });
    const props = {
      server,
      existingNames: new Set(["remote"]),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:remote",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), {
      target: { value: JSON.stringify({ type: "sse", url: "https://b.test", headers: { Authorization: "typed" } }) },
    });
    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);

    expect(
      JSON.parse((screen.getByLabelText("MCP server JSON configuration") as HTMLTextAreaElement).value),
    ).toEqual({ type: "sse", url: "https://b.test" });
  });

  describe("JSON drafts that are not a finished server yet", () => {
    const setup = async (server: ReturnType<typeof managedServer> | null, draftKey: string) => {
      const props = {
        server,
        existingNames: new Set<string>(server ? [server.name] : []),
        onSave: vi.fn().mockResolvedValue(undefined),
        onOpenChange: vi.fn(),
        draftKey,
      };
      const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
      if (!server) await userEvent.setup().click(screen.getByRole("tab", { name: "JSON" }));
      const type = (value: string) =>
        fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), { target: { value } });
      const reopen = () => {
        view.rerender(<McpServerDialog open={false} {...props} />);
        view.rerender(<McpServerDialog open {...props} />);
        return (screen.getByLabelText("MCP server JSON configuration") as HTMLTextAreaElement).value;
      };
      return { type, reopen, props };
    };

    it("never stores a credential nested in an array", async () => {
      const { type } = await setup(null, "agent-1:arr");
      type('{"url":"https://example.test","options":[{"token":"s3cret-nested"}]}');
      expect(JSON.stringify(useMcpDialogDraftStore.getState())).not.toContain("s3cret-nested");
    });

    it("keeps valid JSON that has no url or command yet", async () => {
      const { type, reopen } = await setup(null, "agent-1:notarget");
      type('{"type":"sse","note":"half done"}');
      expect(JSON.parse(reopen())).toEqual({ type: "sse", note: "half done" });
    });

    it.each([
      ['{"url": "https://example.test", "headers": {"Authorization": "Bearer s3cret-partial"', "example.test"],
      ['{"password": abc, region: synthetic-s3cret', "region"],
    ])("stores none of half-typed JSON: %s", async (text, field) => {
      const { type, reopen } = await setup(null, "agent-1:partial");
      type(text);
      const stored = JSON.stringify(useMcpDialogDraftStore.getState());
      expect(stored).not.toContain("s3cret");
      expect(stored).not.toContain(field);
      const restored = reopen();
      expect(restored).not.toContain("s3cret");
      expect(restored).not.toContain(field);
    });

    it("keeps the last complete JSON while a later edit is half-typed", async () => {
      const { type, reopen } = await setup(null, "agent-1:last-complete");
      type('{"type":"sse","url":"https://example.test"}');
      type('{"type":"sse","url":"https://example.test","token": s3cret-half, x');
      const restored = reopen();
      expect(JSON.parse(restored)).toEqual({ type: "sse", url: "https://example.test" });
    });

    it("keeps the last complete JSON after the editor is cleared", async () => {
      const { type, reopen } = await setup(null, "agent-1:cleared");
      type('{"type":"sse","url":"https://example.test"}');
      type("");
      expect(JSON.parse(reopen())).toEqual({ type: "sse", url: "https://example.test" });
    });

    it("drops a saved credential inside an array when the destination moved", async () => {
      const server = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://a.test", options: [{ token: "saved", name: "a" }] },
        transport: "sse",
      });
      const { type, reopen, props } = await setup(server, "agent-1:remote-array");
      type('{"type": "sse", "url": "https://b.test", "options": [{"token": "typed-s3cret", "name": "b"}]}');
      const restored = reopen();
      expect(restored).not.toContain("typed-s3cret");
      await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
      await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
      expect(props.onSave).toHaveBeenCalledWith("remote", {
        type: "sse",
        url: "https://b.test",
        options: [{ name: "b" }],
      });
    });

    it("keeps the saved credentials of an edited server through draft, reopen and save", async () => {
      const server = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://a.test", headers: { Authorization: "saved" } },
        transport: "sse",
      });
      const { type, reopen, props } = await setup(server, "agent-1:remote-partial");
      type('{"type": "sse", "url": "https://b.test", "headers": {"Authorization": "typed-s3cret"}}');
      const restored = reopen();
      expect(restored).not.toContain("typed-s3cret");
      await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
      await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
      expect(props.onSave).toHaveBeenCalledWith("remote", { type: "sse", url: "https://b.test" });
    });

    it("does not bring a removed credential back, on the same or a new destination", async () => {
      const server = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://a.test", headers: { Authorization: "saved" } },
        transport: "sse",
      });
      const { type, reopen, props } = await setup(server, "agent-1:remote-removed");
      type('{"type": "sse", "url": "https://b.test"}');
      expect(JSON.parse(reopen())).toEqual({ type: "sse", url: "https://b.test" });
      await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
      await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
      expect(props.onSave).toHaveBeenCalledWith("remote", { type: "sse", url: "https://b.test" });
    });

    it("does not bring a removed credential back when only the credential was removed", async () => {
      const server = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://a.test", headers: { Authorization: "saved" } },
        transport: "sse",
      });
      const { type, reopen } = await setup(server, "agent-1:remote-removed-only");
      type('{"type": "sse", "url": "https://a.test", "note": "x"}');
      expect(JSON.parse(reopen())).toEqual({ type: "sse", url: "https://a.test", note: "x" });
    });

    it("keeps the saved credential while the destination and credential are untouched", async () => {
      const server = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://a.test", headers: { Authorization: "saved" } },
        transport: "sse",
      });
      const { type, reopen } = await setup(server, "agent-1:remote-kept");
      type('{"type": "sse", "url": "https://a.test", "headers": {"Authorization": "saved"}, "note": "x"}');
      expect(JSON.parse(reopen())).toEqual({
        type: "sse",
        url: "https://a.test",
        headers: { Authorization: "saved" },
        note: "x",
      });
    });
  });

  it("does not carry saved headers to a new URL typed in the form", async () => {
    const server = managedServer({
      name: "remote",
      config: { type: "http", url: "https://a.test", headers: { Authorization: "saved" } },
      transport: "http",
    });
    const props = {
      server,
      existingNames: new Set(["remote"]),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:remote-form",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    fireEvent.change(screen.getByLabelText("Server URL"), { target: { value: "https://b.test" } });
    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);
    await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
    await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
    expect(props.onSave).toHaveBeenCalledWith("remote", { type: "http", url: "https://b.test" });
  });

  it("does not carry a saved oauth secret to a new URL typed in the form", async () => {
    const server = managedServer({
      name: "remote",
      config: {
        type: "http",
        url: "https://a.test",
        headers: { Authorization: "saved" },
        oauth: { clientSecret: "saved-client-secret" },
        note: "keep",
      },
      transport: "http",
    });
    const props = {
      server,
      existingNames: new Set(["remote"]),
      onSave: vi.fn().mockResolvedValue(undefined),
      onOpenChange: vi.fn(),
      draftKey: "agent-1:remote-oauth",
    };
    const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
    fireEvent.change(screen.getByLabelText("Server URL"), { target: { value: "https://b.test" } });
    view.rerender(<McpServerDialog open={false} {...props} />);
    view.rerender(<McpServerDialog open {...props} />);
    await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
    await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
    expect(props.onSave).toHaveBeenCalledWith("remote", { type: "http", url: "https://b.test", note: "keep" });
  });

  describe("a draft made against an older saved server", () => {
    it("does not give the newer saved token to a form draft on another URL", async () => {
      const old = managedServer({ name: "remote", config: { type: "http", url: "https://a.test" }, transport: "http" });
      const props = {
        server: old,
        existingNames: new Set(["remote"]),
        onSave: vi.fn().mockResolvedValue(undefined),
        onOpenChange: vi.fn(),
        draftKey: "agent-1:remote-stale-form",
      };
      const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
      fireEvent.change(screen.getByLabelText("Server URL"), { target: { value: "https://b.test" } });
      view.rerender(<McpServerDialog open={false} {...props} />);
      const newer = managedServer({
        name: "remote",
        config: {
          type: "http",
          url: "https://c.test",
          headers: { Authorization: "newer-token" },
          oauth: { clientSecret: "newer-secret" },
        },
        transport: "http",
      });
      view.rerender(<McpServerDialog open {...props} server={newer} />);
      await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
      await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
      expect(props.onSave).toHaveBeenCalledWith("remote", { type: "http", url: "https://b.test" });
    });

    it("does not give the newer saved token to a JSON draft on another URL", async () => {
      const old = managedServer({ name: "remote", config: { type: "sse", url: "https://a.test" }, transport: "sse" });
      const props = {
        server: old,
        existingNames: new Set(["remote"]),
        onSave: vi.fn().mockResolvedValue(undefined),
        onOpenChange: vi.fn(),
        draftKey: "agent-1:remote-stale-json",
      };
      const view = render(<McpServerDialog open {...props} />, { wrapper: Wrapper });
      fireEvent.change(screen.getByLabelText("MCP server JSON configuration"), {
        target: { value: '{"type": "sse", "url": "https://b.test", "note": "x"}' },
      });
      view.rerender(<McpServerDialog open={false} {...props} />);
      const newer = managedServer({
        name: "remote",
        config: { type: "sse", url: "https://c.test", headers: { Authorization: "newer-token" } },
        transport: "sse",
      });
      view.rerender(<McpServerDialog open {...props} server={newer} />);
      await userEvent.setup().click(screen.getByRole("button", { name: /save/i }));
      await vi.waitFor(() => expect(props.onSave).toHaveBeenCalled());
      expect(props.onSave).toHaveBeenCalledWith("remote", { type: "sse", url: "https://b.test", note: "x" });
    });
  });
});
