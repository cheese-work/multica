import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { RowActionsMenu, RowContextMenu } from "./row-actions-menu";

describe("RowActionsMenu", () => {
  it("names its trigger so a screen reader can announce the row's actions", () => {
    render(
      <RowActionsMenu
        label="Chat actions"
        groups={[[{ key: "archive", icon: null, label: "Archive", onSelect: vi.fn() }]]}
      />,
    );

    expect(screen.getByRole("button", { name: "Chat actions" })).toBeInTheDocument();
  });

  it("runs the action the user picks", async () => {
    const onSelect = vi.fn();
    render(
      <RowActionsMenu
        label="Chat actions"
        groups={[[{ key: "archive", icon: null, label: "Archive", onSelect }]]}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Chat actions" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Archive" }));

    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it("stays out of the row's own click handling", () => {
    // The trigger sits inside a row that selects on click. Opening the menu
    // must not also select the row.
    const onRowClick = vi.fn();
    render(
      <div onClick={onRowClick}>
        <RowActionsMenu
          label="Chat actions"
          groups={[[{ key: "archive", icon: null, label: "Archive", onSelect: vi.fn() }]]}
        />
      </div>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Chat actions" }));

    expect(onRowClick).not.toHaveBeenCalled();
  });

  it("renders nothing when every group is empty", () => {
    // A running chat with no pending task has no action to offer; an empty
    // trigger would be a dead control.
    const { container } = render(<RowActionsMenu label="Chat actions" groups={[[], []]} />);

    expect(container).toBeEmptyDOMElement();
  });
});

describe("RowActionsMenu shortcut hints and reveal", () => {
  const groups = [
    [
      { key: "pin", icon: null, label: "Pin", shortcut: "P", onSelect: vi.fn() },
      { key: "archive", icon: null, label: "Archive", shortcut: "E", onSelect: vi.fn() },
    ],
  ];

  it("shows each item's shortcut hint", async () => {
    render(<RowActionsMenu label="Chat actions" groups={groups} />);

    fireEvent.click(screen.getByRole("button", { name: "Chat actions" }));

    const pin = await screen.findByRole("menuitem", { name: /Pin/ });
    expect(pin).toHaveTextContent("P");
    expect(screen.getByRole("menuitem", { name: /Archive/ })).toHaveTextContent("E");
  });

  it("with revealOnHover the trigger waits for row hover or focus where hover exists", () => {
    render(<RowActionsMenu label="Chat actions" groups={groups} revealOnHover />);

    const cls = screen.getByRole("button", { name: "Chat actions" }).className;
    expect(cls).toContain("[@media(hover:hover)]:invisible");
    expect(cls).toContain("[@media(hover:hover)]:group-hover/row:visible");
    expect(cls).toContain("[@media(hover:hover)]:group-focus-within/row:visible");
    expect(cls).not.toContain("[@media(hover:hover)]:hidden");
  });

  it("gives the trigger a 2px focus-visible ring", () => {
    render(<RowActionsMenu label="Chat actions" groups={groups} revealOnHover />);

    expect(screen.getByRole("button", { name: "Chat actions" }).className).toContain(
      "focus-visible:ring-2",
    );
  });
});

describe("RowContextMenu", () => {
  it("opens the same items on right-click", async () => {
    const onSelect = vi.fn();
    render(
      <RowContextMenu
        groups={[
          [{ key: "pin", icon: null, label: "Pin", shortcut: "P", onSelect }],
          [{ key: "rename", icon: null, label: "Rename", shortcut: "R", onSelect: vi.fn() }],
        ]}
      >
        <div data-testid="row">row</div>
      </RowContextMenu>,
    );

    fireEvent.contextMenu(screen.getByTestId("row"));

    expect(await screen.findByRole("menuitem", { name: /Pin/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Rename/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: /Pin/ }));
    expect(onSelect).toHaveBeenCalledTimes(1);
  });
});
