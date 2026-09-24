# Industrial Control Room Dashboard Implementation Plan

**Goal:** Redesign the existing SCADATR monitoring dashboard into a dark industrial control-room interface and add an automatically refreshed daily report area with manual clipboard copy.

**Architecture:** Keep the existing vanilla HTML/CSS/JavaScript stack and all REST endpoints. Replace the page shell and visual system in `web/index.html` and `web/style.css`, then extend `web/app.js` with report rendering, clipboard copy, refresh state, and safer field handling for existing lists and history.

**Tech Stack:** Semantic HTML, vanilla CSS, browser Clipboard API with a textarea fallback, existing Go REST API.

## Global Constraints

- Keep the existing API routes and localStorage API-key behavior.
- Do not add frontend dependencies or migrate frameworks.
- Preserve recipient, schedule, session, WhatsApp, QR, modal, and auto-refresh behavior.
- Use a charcoal/amber industrial palette without purple gradients, thick black borders, or generic card repetition.
- Keep report data sourced from `/api/report/latest`.

### Task 1: Rebuild the dashboard structure

**Files:**
- Modify: `web/index.html`

- [ ] Replace the placeholder page shell with a semantic top bar, monitoring workspace, report surface, telemetry surface, history table, operational controls, and status side rail.
- [ ] Preserve every existing JavaScript hook ID and add `daily-report`, `report-state`, `report-generated-at`, `btn-copy-report`, and `btn-refresh-report`.
- [ ] Add accessible labels, live regions, modal semantics, and metadata for the report and connection states.

### Task 2: Apply the industrial visual system

**Files:**
- Modify: `web/style.css`

- [ ] Define charcoal surfaces, amber signal colors, muted green/red states, compact typography, data-metric numerals, and responsive grid rules.
- [ ] Add hover, focus, pressed, disabled, loading, empty, and error states for the controls.
- [ ] Make desktop and mobile layouts usable without changing application behavior.

### Task 3: Wire the daily report and polish existing behavior

**Files:**
- Modify: `web/app.js`

- [ ] Render the latest report automatically whenever report data refreshes.
- [ ] Add clipboard copy with a secure Clipboard API path and a textarea fallback.
- [ ] Add refresh timestamp and refresh-in-progress state.
- [ ] Preserve existing API interactions and fix history/schedule ID matching to accept the actual JSON field names.
- [ ] Escape dynamic text inserted into HTML list/table templates.

### Task 4: Verify the frontend change

**Files:**
- Verify: `web/index.html`
- Verify: `web/style.css`
- Verify: `web/app.js`

- [ ] Run JavaScript syntax checks and the Go test suite.
- [ ] Start a local static server when possible and inspect desktop/mobile layout.
- [ ] Exercise report copy, report empty state, history modal, refresh control, and existing controls.
