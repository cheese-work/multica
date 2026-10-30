## Workflow editor evidence

Before = base #417 head `472e046f4` (read-only graph). After = this PR head. Same synthetic workflow, light and dark, 1440 and 390 px.

Capture is an X99 Chromium harness around the real `WorkflowEditorPage`; save is stubbed. Visual diff: NOT-RUN.

`browser-base-417.txt` and `browser-drag.txt` hold the browser observations.

### Defect found by the browser run

On PR head `7f780b6d6` the graph showed 7 nodes, all `visibility: hidden`, and 0 edges (`{"nodes":7,"visibleNodes":0,"edges":0}`). The graph dropped React Flow `dimensions` changes, so controlled nodes never counted as measured. jsdom tests cannot see this. Fixed in `578769e08`: `{"nodes":7,"visibleNodes":7,"edges":6}`. The base #417 graph is not affected.

### Real-browser drag

Dragging the `todo` source handle onto the `done` target handle added the exit (6 to 7 edges). Dragging the Blocked node moved it 92 px. Save then sent `todo.edges = [in_progress/agent_done, done/agent_done]`.
