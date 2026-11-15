// Shared vi.mock factories for the form-screen tests. Leaf components are
// reduced to DOM so the screens' own wiring (restored gating, close and
// settle behavior) runs for real.
import type { ReactNode } from "react";
import { vi } from "vitest";

export const router = { back: vi.fn(), push: vi.fn() };

export const expoRouter = () => ({
  router,
  useLocalSearchParams: () => ({ id: "rec1" }),
  Stack: {
    Screen: ({ options }: { options: Record<string, () => ReactNode> }) => (
      <header>
        {options.headerLeft?.()}
        {options.headerRight?.()}
      </header>
    ),
  },
});

export const text = () => ({
  Text: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
});

export const autosizeTextArea = () => ({
  AutosizeTextArea: (p: {
    value: string;
    onChangeText: (v: string) => void;
    editable?: boolean;
    placeholder?: string;
  }) => (
    <textarea
      value={p.value}
      placeholder={p.placeholder}
      readOnly={p.editable === false}
      onChange={(e) => p.onChangeText(e.target.value)}
    />
  ),
});

export const i18n = () => ({
  useT: () => ({ t: (k: string) => k }),
});
