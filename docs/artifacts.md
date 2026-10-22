# Artifacts

An artifact is one self-contained thing an agent or a person made: a page, a component, a document, a deck, a diagram or a source file. Every save is a numbered, immutable version.

## Kinds

| Kind       | Source format                                  | Preview                        |
| ---------- | ---------------------------------------------- | ------------------------------ |
| `html`     | Self-contained HTML, inline CSS and JavaScript | Interactive webpage            |
| `react`    | JSX or TSX, default-export a component         | Interactive React 19 component |
| `markdown` | Markdown, fenced `mermaid` blocks, `$TeX$` math | Formatted document            |
| `slides`   | Markdown slides separated by `---` lines       | 16:9 slide deck                |
| `svg`      | SVG markup                                     | Vector graphic                 |
| `mermaid`  | Mermaid diagram source                         | Rendered diagram               |
| `code`     | Any text                                       | Syntax-highlighted source      |

React is TSX compiled locally by esbuild, and may import the bundled libraries listed below. Other package imports, filesystem imports, and URL imports are rejected with a visible preview error (and a `warnings` entry for agents). Style with Tailwind classes, inline CSS, or `<style>`. This version handles single-file artifacts, not multi-file app projects, arbitrary npm installations, or server-side execution.

All previews use an opaque-origin iframe sandbox, with a response CSP that also applies when opening a preview URL directly. Scripts can run inside the artifact, submit forms to their own handlers, use `alert`/`confirm`/`prompt`, start downloads, lock the pointer, and open links in a new tab. The frame may also ask for the camera, microphone, location, screen capture and clipboard writes; the browser prompts you, and these need a secure context (`localhost` or HTTPS, such as a TLS proxy). They cannot read the workspace or its cookies and localStorage, load nested frames, or fetch API or external resources; forms cannot be sent anywhere (`form-action 'none'`). Uploaded media can load from `/media/` URLs; images, media, and fonts can also use data URLs, and fonts the bundled `/vendor/fonts`; video/audio can use blob URLs, as can workers. External media URLs and embedded players such as YouTube remain blocked. Markdown HTML is confined to the same sandbox. A link click opens a new top-level tab outside the sandbox, so an artifact can send you to any site when you click; it still cannot make background requests. CSP and sandboxing constrain content capabilities; they do not provide CPU/memory isolation from a deliberately expensive browser program.

## Editing

- Switch between Preview, Split, and Source. **Edit source** enables the editor; previews update after a short pause. **Save version** or **Cmd/Ctrl+S** creates an immutable revision.
- Drafts are kept in this browser's localStorage and recovered on reload. Save them to persist on the server. **Discard draft** removes a local draft explicitly. Browsers that block localStorage show a notice.
- Choose an older version to inspect it or download its exact source (a zip of the page and its files when it has any). **Restore** appends a new revision from that version, files included; it preserves history.
- **Changes** shows a line diff of the selected version against the one before it (or any earlier version), file by file, with unchanged stretches folded. While editing it compares your unsaved draft with the latest saved version.
- With files, the source pane has a file switcher; in edit mode **+ File** adds one and **Remove** deletes the one shown.
- **Copy link** shares a URL pinned to the selected version. `/a/{id}` without a version follows the latest saved version. **Open ↗** opens the full-page view at `/view/{id}`. Shared links have the same network access as the app, and are not permission boundaries.
- The more menu lets you pin an artifact to the top of the library, duplicate it, or delete it and its history (with its storage and comments). Delete is permanent and requires confirmation. Concurrent changes also block stale deletes.
- **Description** (in the editor, or `description` from an agent) is a one-line subtitle shown under the title, on the home page's **Recently made** cards, and in library search. **Icon** is one generic word such as `chart`, `recipe`, `map` or `timer` (or an emoji), shown in the library, on cards, and as the browser-tab icon. **Tags** are short labels (`finance`, `recipes`, `work`), typed with commas; the editor suggests some from the artifact's own words. None of these are versioned.
- **Copy source** in the more menu copies the file the source pane shows.
- **Recently made** cards show a live, scaled-down render of each artifact, loaded as they scroll into view and unloaded when you open an artifact. Thumbnails read `window.storage`, so they show real state, but their writes are not saved and they cannot call Claude.
- The phone layout keeps the library above the workspace. The workspace follows the system light or dark theme until you pick one with the sun/moon switch (the library footer, the apps bar, or the view bar). The choice is remembered in this browser, and artifacts keep their own styling.

## Finding things

Library search reads everything an artifact says, not just its title: the rendered text of HTML, Markdown and slides, the words a React app shows, SVG and diagram labels, supporting files, descriptions, tags, version notes and comments. It runs on the server with no external service.

- **Typos and word forms are fine.** `bugdet` finds budget, `recipe` finds recipes, `planning` finds planned, and accents fold (`pokemon` finds Pokémon). When a word matched nothing as typed, **Did you mean…** offers the correction. The last word matches as a prefix while you type.
- **It knows a few everyday synonyms**, such as chart and graph, video and movie, slides and presentation, budget and money.
- **Find things by what they are.** Results come from what an artifact contains (video, audio, images, charts, diagrams, tables, forms, math, 3D, data) as well as what it says: search `video`, or pick it from the **Contains…** menu.
- **Filters**, typed or picked: `kind:react` (or the type menu), `tag:finance` or `#finance` (or the tag chips under the search box), `is:pinned`, `has:comments`, `has:open` (unresolved threads), `has:files`, `has:chart`, `updated:today|week|month|year|30d|2026-03|>2026-01-01` (or the time menu), `created:…`, `before:` and `after:` dates, `"exact phrase"` and `-word` to exclude.
- **Results** are ranked by where words appear (title, then tags, description, headings and text), each with a highlighted excerpt.
- **Related** under **Details** lists artifacts that share an artifact's most telling words, with the words they share.
- **Keyboard:** `/` or `Cmd/Ctrl+K` focuses search, `Enter` opens the best match, and the arrow keys move through the results.

Agents get the same search through `search_artifacts`, and `GET /api/search?q=` serves it over HTTP.

## Default design

The workspace uses the shared warm editorial system: `#faf9f5` cream canvas, `#cc785c` coral primary actions, `#141413` warm ink, `#efe9de` cream cards and `#181715` dark code surfaces. Regular-weight serif headings with negative tracking pair with humanist sans body text, and code uses JetBrains Mono. The stacks prefer the licensed Copernicus/Tiempos and StyreneB. The server bundles their open substitutes, Cormorant Garamond (its 500 cut), Inter and JetBrains Mono (OFL, at `/vendor/fonts`), so nothing remote is fetched. Coral is kept scarce, depth comes from color blocks rather than shadows, and only the primary button changes state, darkening when pressed. Markdown, slides, diagrams and code artifacts render in the same system.

The **`get_design_system`** MCP tool returns the same CSS file used by the workspace, the full design system as authoring rules (color, type scale, spacing, radii, components, do's and don'ts, breakpoints) and a self-contained HTML starter. MCP initialization and artifact tool descriptions instruct agents to use it for **new visual artifacts unless the user requests another style**. Existing designs stay intact during edits unless restyling is requested. This is agent guidance: the server does not inject styles into saved source or alter downloads. Copy the CSS into the artifact to use it; the returned starter already includes it. Claude/Anthropic branding is not part of artifact defaults.

## How artifacts are stored

Each artifact is one JSON file in the data directory, including all revisions, its description, tags, pin, owner and declared capabilities. Its `db` documents live in `db/{id}.json` and its assets in `assets/{id}/`; people in `meta/people.json`. Its `window.storage` data lives in `storage/{id}.json` and its comments in `comments/{id}.json`; uploaded media in `media/`, with upload names in `media/names.json`. Writes use a temporary file, fsync, and atomic rename. The server refuses corrupt saved files on startup instead of silently replacing them. Source content is limited to 2 MiB per revision (supporting files, stored in the revision, to 100 files of 2 MiB and 8 MiB in all); titles to 200 UTF-8 bytes, notes to 1000 bytes. Data is stored unencrypted; use disk encryption if needed.

Back up the data directory (or Docker volume). For a consistent multi-artifact snapshot, stop the service before copying it. Restore the files into the configured data directory and start the service. Run exactly one server process per data directory; the concurrency lock is process-local. Disk usage grows with revision history.
