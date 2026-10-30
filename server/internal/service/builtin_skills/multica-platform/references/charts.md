# Charts and files in a comment

Where content goes decides how it shows:

- **In the body, rendered in place** — a fenced ` ```html ` or ` ```mermaid `
  block in the comment content. It renders inside the comment with a title
  bar (Preview / Source, fullscreen, copy) and takes its content's height;
  anything taller than 480px collapses behind "Show all". Name it with
  `title="..."` on the fence line. HTML runs in a scripts-only sandbox (no
  cookies, storage or parent access; CDN `<script src>` works).
- **An attached file** — `--attachment <path>`. Every non-image file shows as
  a file card that opens in the viewer, **HTML included**: an uploaded
  `report.html` is a deliverable to open, not an inline chart. Use it for
  something the reader keeps or downloads.

For HTML that should follow light / dark mode, style it with the page's theme
variables: `var(--background)`, `var(--foreground)`, `var(--muted)`,
`var(--muted-foreground)`, `var(--border)`, `var(--primary)`,
`var(--chart-1)` … `var(--chart-5)`, `var(--font-sans)`. Using any of them opts
the block into the app's color scheme, so also set the page background
(`body { background: var(--background); color: var(--foreground) }`). HTML
that uses none keeps its own look. Size to the content, not the viewport:
`100vh` heights have no fixed viewport to fill here.

````markdown
```html title="p95 latency, last 7 days"
<canvas id="c"></canvas>
<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
<script>/* draw with getComputedStyle(document.documentElement)
  .getPropertyValue("--chart-1") so it follows the theme */</script>
```
````
