// Minimal DOM stand-ins for the react-native pieces the form screens use, so
// vitest can render the real screens under jsdom. Behavior only: no layout, no
// native views.
import { vi } from "vitest";
import type { ReactNode } from "react";

type Props = Record<string, unknown> & { children?: ReactNode };

export const View = ({ children }: Props) => <div>{children}</div>;
export const ScrollView = View;
export const KeyboardAvoidingView = View;
export const Platform = { OS: "ios" };
export const Alert = { alert: vi.fn() };
export const InteractionManager = { runAfterInteractions: (fn: () => void) => fn() };

export const Pressable = ({ children, onPress, disabled }: Props) => (
  <button
    type="button"
    disabled={Boolean(disabled)}
    onClick={onPress as () => void}
  >
    {children}
  </button>
);

export const TextInput = ({
  value,
  onChangeText,
  editable = true,
  placeholder,
}: Props) => (
  <input
    value={value as string}
    placeholder={placeholder as string}
    readOnly={editable === false}
    onChange={(e) => (onChangeText as (v: string) => void)(e.target.value)}
  />
);
