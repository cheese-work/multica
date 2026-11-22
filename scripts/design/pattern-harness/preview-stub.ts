// The harness never opens the attachment preview modal; the real one pulls the whole markdown/code renderer.
export function useAttachmentPreview() {
  return { modal: null, open: () => {}, tryOpen: () => false };
}
// The editor barrel re-exports these by name; the build needs them, the capture never calls them (CHE-5538).
export const AttachmentPreviewModal = () => null;
export const isPreviewable = () => false;
