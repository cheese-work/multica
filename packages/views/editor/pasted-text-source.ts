// Kept free of Tiptap imports: use-coordinated-uploads (chat composer) needs these
// before the editor chunk loads, and must not pull the engine into the route (CHE-3018).

/** Filename given to the .txt synthesised from an over-threshold paste. */
export const PASTED_TEXT_FILENAME = "pasted-text.txt";

/**
 * Source text of every file synthesised by the paste-as-file path, keyed by
 * the File instance that carries it downstream.
 *
 * Unlike a dropped file, this one has no copy anywhere else: the text was
 * never written into the document and its source may be a tab the user has
 * already closed. So whoever owns the draft must be able to put it back when
 * the upload fails. A WeakMap rather than a property on the File keeps the
 * File exactly as the upload layer expects it, and lets the entry die with the
 * upload. Read it with {@link pastedTextSource}; a File that came from disk
 * returns undefined and needs no recovery.
 */
const pastedTextSources = new WeakMap<File, string>();

/** Record the text a synthesised paste file was built from. */
export function markPastedTextFile(file: File, text: string): File {
  pastedTextSources.set(file, text);
  return file;
}

/** The text an over-threshold paste was made from, or undefined for real files. */
export function pastedTextSource(file: File): string | undefined {
  return pastedTextSources.get(file);
}
