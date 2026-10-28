"use client";

import { useState } from "react";
import { Code, Copy, Download, RotateCw } from "lucide-react";
import { copyText } from "@multica/ui/lib/clipboard";
import { MediaLinkAction, ViewerActions } from "../media/media-context";
import { CodeBlockIframe } from "./code-block-iframe";
import { CodeBlockStatic } from "./code-block-static";
import { downloadBlob } from "./utils/mermaid-export";
import { useT } from "../i18n";

export function HtmlViewer({ html, document, title, onReload }: { html: string; document: string | null; title: string; onReload: () => void }) {
  const { t } = useT("editor");
  const [source, setSource] = useState(false);
  const [reload, setReload] = useState(0);
  const actionClass = "flex size-8 items-center justify-center rounded-md hover:bg-secondary";
  return <div data-testid="media-stage" className="flex h-full min-h-0 flex-col">
    <header data-testid="media-toolbar" className="flex shrink-0 flex-wrap items-center gap-2 border-b border-border px-3 py-2">
      <h1 className="mr-auto w-full truncate text-body font-medium md:w-auto">{title}</h1>
      <ViewerActions>
        <button type="button" aria-label={t(($) => $.attachment.reload)} className={actionClass} onClick={() => { onReload(); setReload((value) => value + 1); }}><RotateCw className="size-4" /></button>
        <button type="button" aria-label={t(($) => $.attachment.view_source)} aria-pressed={source} className={actionClass} onClick={() => setSource(!source)}><Code className="size-4" /></button>
        <button type="button" aria-label={t(($) => $.dynamic_block.copy_source)} className={actionClass} onClick={() => void copyText(html)}><Copy className="size-4" /></button>
        <button type="button" aria-label={t(($) => $.image.download)} className={actionClass} onClick={() => downloadBlob(new Blob([html], { type: "text/html" }), "preview.html")}><Download className="size-4" /></button>
        <MediaLinkAction kind="html" content={html} />
      </ViewerActions>
    </header>
    {source ? <div className="min-h-0 flex-1 overflow-auto p-4"><CodeBlockStatic language="html" body={html} /></div> : document != null ? <CodeBlockIframe key={reload} html={document} title={title} heightClassName="min-h-0 flex-1" className="rounded-none border-0" /> : <p className="p-4">{t(($) => $.attachment.preview_loading)}</p>}
  </div>;
}
