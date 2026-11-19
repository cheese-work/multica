## View popover and unread marker (B-c) · doc — direction B, frame `cMNzhLglth` `updatedAt` `2026-11-18T15:32:15.696Z`

Copied unchanged from doc frame `70b40Q0Os8` (`updatedAt` `2026-11-18T15:32:27.546Z`). Frame B-c, desktop popover and phone sheet. Settings for the view line. Cheese kept the unread marker (CHE-6441) and dropped mute, pin and the one-agent filter.

### Purpose
Let you change the view for this issue only (Focus, Follow or All), set your default view, and turn the unread marker on or off.

### UX reasoning
Three presets cover the real needs: Focus (only what is with you), Follow (everything, agent-only folded) and All (nothing folded). A change on this issue never alters your default; "Reset to my default" is always one click away. The popover holds three settings below the presets: two that already existed ("Count FYI mentions as with me" and "Sort: waiting first") and the unread marker, the one new control Cheese kept. Follow stays the default for every user.

### UI reasoning
A popover on desktop and a bottom sheet on phone. Both have a scope switch ("All my issues" or "This issue only"), the three preset rows, and the same switches on both sizes: "Count FYI mentions as with me", "Sort: waiting first" and, on the default scope, the unread marker (the override view leaves out the unread marker because it is a person setting). The selected preset has a filled radio, a tinted row and a text label, so the choice does not rest on colour. Components come from the Components page: view-row, switch, popover. Tokens come from the CHE-6385 tokens frame.

### Ergonomics
Preset rows are at least 56 px high on both sizes, and the phone sheet sits in the lower half, so a thumb reaches every row. Switch rows are at least 44 px high with a larger hit area than the switch. The phone scope switch and Done are 44 px. Desktop has Cancel and Done; Esc closes the popover. The prototype closes the sheet with Done.

### States
Follow selected (default), Focus selected, All selected, switch on and off, and the result frames B-d (Focus) and B-e (All). The switch shows default, hover, focus and disabled on the Components page.

### Motion
The popover opens in 150 ms (opacity and an 8 px rise); the phone sheet slides up in 200 ms. A chosen preset dissolves the thread list in 150 ms. The switch moves in 120 ms. With reduced motion, all of these are instant swaps, and the scrim appears at once.

### Accessibility
The popover is `role=dialog` with a name and focus moves into it on open; Esc closes it and focus returns to View. Presets are rows in one group, with the chosen row marked `aria-pressed` and a text chip ("Default" or "This issue only"). Each switch is `role=switch` with `aria-checked` and a label, "Unread marker". Targets on phone are at least 44 px.
