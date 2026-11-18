export function usePreviewSequence() {
  return { openAt: () => false };
}
// Named re-exports of the editor barrel (CHE-5538); never called in a capture.
export const PreviewSequenceProvider = ({ children }: { children?: unknown }) => children as never;
export const collectPreviewSequence = () => [];
