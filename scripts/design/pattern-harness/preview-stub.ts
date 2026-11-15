// The harness never opens the attachment preview modal; the real one pulls the whole markdown/code renderer.
export function useAttachmentPreview() {
  return { modal: null, open: () => {}, tryOpen: () => false };
}
