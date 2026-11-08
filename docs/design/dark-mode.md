# Dark mode

The design system (`web/design-system.css`, handed to agents by `get_design_system`)
has a light and a dark side. Artifacts built on it follow the viewer's light/dark
choice, and so do the pages shartifacts renders itself: Markdown, slides, diagrams and
code. This page is the specification: how the modes are expressed, who chooses between
them, and the rules a design must keep in both.

## Tokens

Every color token is written once, as `light-dark(light, dark)`:

```css
:root {
  color-scheme: light dark;
  --canvas: light-dark(#f9f8f6, #1c1a19);
  --ink: light-dark(#1a1716, #f5f1ee);
  /* … */
}
```

The root's `color-scheme` picks the side, so a component styled with tokens needs no
dark rules of its own, and the two modes cannot drift apart: a token added without a
dark value is visibly the same in both. Tokens whose value is the same in both modes
(`--primary`, `--on-primary`, `--on-dark`) are plain colors. The workspace's other
palettes (`web/palettes.css`) are written the same way.

| Role                 | Token                    | Light     | Dark          |
| -------------------- | ------------------------ | --------- | ------------- |
| Page floor           | `--canvas`               | `#f9f8f6` | `#1c1a19`     |
| Soft band            | `--surface-soft`         | `#f2efeb` | `#232120`     |
| Feature card         | `--surface-card`         | `#ebe6e0` | `#2b2826`     |
| Selected / emphasis  | `--surface-cream-strong` | `#e3dcd4` | `#36322f`     |
| Dark band            | `--surface-dark`         | `#1c1a19` | `#141211`     |
| Dark band edge       | `--surface-dark-edge`    | none      | `#3c3734`     |
| Border               | `--hairline`             | `#e6e0d9` | `#3c3734`     |
| Headlines            | `--ink`                  | `#1a1716` | `#f5f1ee`     |
| Running text         | `--body`                 | `#403b38` | `#cec6c1`     |
| Secondary text       | `--muted`                | `#6e6863` | `#a69e98`     |
| Primary fill         | `--primary`              | `#b5523b` | `#b5523b`     |
| Primary as text      | `--primary-text`         | `#b5523b` | `#e8917a`     |
| Status as text       | `--success-text` …       | darkened  | lightened     |
| Small shadow         | `--shadow-sm`            | 8% ink    | 40% black     |
| Focus ring           | `--focus-ring`           | 15% brick | 35% brick     |

The full list, with every value, is `designRules` in `internal/server/mcp.go`, which
is what agents read.

## Who chooses

From weakest to strongest:

1. **The system.** `color-scheme: light dark` follows `prefers-color-scheme`. A
   preview opened on its own, or a downloaded artifact, behaves like this.
2. **The page showing the artifact.** The workspace, the full-page view and share
   pages pass their own light/dark choice to the preview as `data-scheme` on its
   `<html>`. An artifact in a sandboxed frame cannot see that choice by itself:
   browsers resolve `prefers-color-scheme` in a frame from the system, not from the
   parent's `color-scheme`, so it is passed in two ways:
   - `web/theme.js` keeps the choice in the `shartifacts-scheme` cookie, and the
     server writes `data-scheme` into the preview's first byte (`previewScheme` in
     `internal/server/preview.go`). The preview paints in the right mode from the
     start, with no flash. Only `light` and `dark` pass.
   - Every preview asks to follow the choice (`shartifacts-theme-request`), and
     `theme.js` answers with a `shartifacts-theme` message then and on every change. The preview runtime sets `data-scheme` and fires
     `shartifacts:scheme` on `document` for pages that draw their own colors (charts,
     canvases, Mermaid).
3. **The author.** `<html data-theme="light">` or `"dark"` pins a mode, for content
   that only makes sense in one (a print proof, a photo light table). The workspace's
   own pages set `data-theme` in the same way.

Printing is always light.

Artifacts saved before this change carry the older, light-only copy of the CSS and stay
light. Unstyled HTML, SVG and React artifacts are left as their authors wrote them.

## Rules

- **The same system at night, not an inversion.** Warm near-blacks, never `#000` or
  cool grays; ink is a warm off-white, never `#fff` for text.
- **Elevation gets lighter as it rises:** canvas, then soft, card, cream-strong. Use
  those steps rather than shadows to separate layers. `--shadow-sm` only sharpens an
  edge.
- **Dark bands sink.** `pre`, `.card-dark` and footers sit below the canvas in dark
  mode, and draw `--surface-dark-edge` as an inset ring
  (`box-shadow: inset 0 0 0 1px var(--surface-dark-edge)`) so they keep their shape.
  The ring is transparent in light mode, and a box-shadow, so it never moves layout.
- **Fills keep their hue, text lightens.** `button.primary`, `.callout` and
  `.badge-primary` stay terracotta with white text in both modes. Terracotta as text,
  links, icons or strokes uses `--primary-text`. Status colors as text use
  `--success-text`, `--warning-text` and `--error-text`. The fills are for dots, bars
  and badges.
- **Contrast.** Text meets WCAG AA (4.5:1, or 3:1 for display type) in both modes. On
  `--canvas` every text token passes in both, except `--muted-soft`, which is for
  non-essential fine print only. On `--surface-card` and `--surface-cream-strong`,
  small text uses `--body` or `--ink`. A new color needs a pair that passes in both.
- **Media.** Photos are left alone. Line art, icons, SVG and chart strokes use
  `currentColor` or tokens. Never bake a light background into an image. Libraries
  that take literal colors read the tokens with `getComputedStyle` and redraw on
  `shartifacts:scheme`.
- **Never inline hex.** Anything inline-styled with a literal color is wrong in one
  of the two modes.

## The pages shartifacts renders

Markdown, slides, diagrams and code use the same pairs, inlined as `light-dark()`
values (`documentStyle` in `internal/server/preview.go`, `slidesDeck` in
`slides.go`), with the same scheme rules (`schemeCSS`). Code blocks stay on the dark
surface in both modes, so their highlight colors do not change. Mermaid draws with
its neutral theme in light mode and with the dark tokens in dark mode, and redraws
from its source when the mode changes.

## Not covered

- **Apps** (`/run/`) are not given the workspace's choice; they follow the system.
- **The shadcn/ui theme** for Tailwind React artifacts has its own light palette and
  is unchanged.
