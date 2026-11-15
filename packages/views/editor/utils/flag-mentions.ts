// Marks the rendered mention chips whose id the server refused. Plain DOM:
// the chip is a React node view whose `className` never changes, so React
// leaves the added class alone. Stale flags only live until the next call or
// until the chip is deleted — ponytail: a ProseMirror decoration if flags
// must also clear on edit.
export function flagMentions(root: HTMLElement, ids: string[]): void {
  root.querySelectorAll(".mention-invalid").forEach((el) => el.classList.remove("mention-invalid"));
  root.querySelectorAll<HTMLElement>(".mention[data-mention-id]").forEach((el) => {
    if (ids.includes(el.dataset.mentionId!)) el.classList.add("mention-invalid");
  });
}
