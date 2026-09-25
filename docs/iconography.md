# Interface icons

Use the annotation target as the reference for new Pharos interface icons. The lighthouse app icon and header mark are brand artwork; this guide covers controls, status symbols, and timeline icons.

## Drawing standard

- Draw each icon as original SVG geometry in a `24 × 24` viewBox. Keep the visible shape optically centered, generally within coordinates 3–21.
- Use a `1.6` unit stroke with round caps and joins. The shared `.app-icon` CSS supplies `stroke: currentColor` and `fill: none`; let the surrounding control set the color.
- Prefer a few clear strokes that still read at 14–18 CSS pixels. Avoid heavier outlines, tiny detail, shadows, gradients, emoji, and font glyphs.
- Keep shapes unfilled, including the moon, gear, and state icons. Fill only when the meaning requires a solid mark: `event` is deliberately a small dot (`r="2.2"` on the 24-unit grid). A selected state usually changes color, not the SVG geometry.
- Reuse an existing symbol when it conveys the same action. Give a new action a distinct name and shape rather than relying on a similar Unicode character.

## Size and placement

| Context | Rendered SVG | Placement |
| --- | --- | --- |
| Header controls | 18 × 18 px | Centered in a 32 × 32 px button |
| Ordinary inline controls | 16 × 16 px | Centered with the label; about 7 px gap |
| Dense tables and timeline rows | 14–15 × 14–15 px | Centered in a stable 16 px slot |

Change the rendered size in CSS for the context; keep the SVG's `24 × 24` viewBox and stroke standard. Use `inline-flex` or grid centering on the parent so icon position does not depend on a font baseline. Check the result at its **actual rendered size** in light and dark themes, including hover, selected, and disabled states. Timeline `event` should remain a quiet dot rather than a full-size pictogram.

## Add or use an icon

1. Check the existing `ph-icon-*` symbols in `src/ai_work_archive/ui.py`. Add new artwork to the `<svg class="app-icon-sprite">` block near the start of `<body>` with an ID such as `ph-icon-info`. Paths inherit the shared stroke and fill rules.
2. In the page shell's plain JavaScript, use `icon('info')` for a DOM node. In static HTML, use `<svg class="app-icon" viewBox="0 0 24 24" aria-hidden="true"><use href="#ph-icon-info"/></svg>`.
3. In React, add the name to `IconName` in `web/src/icons.tsx`, then render `<Icon name="info" />`. Put the accessible name on the enclosing button or control; the SVG itself is decorative.
4. If the icon identifies a timeline category, update `categoryIcon()` in `src/ai_work_archive/ui.py`. If it replaces a glyph supplied by query-table-ui, update `iconizeQueryControls()` there. Do not edit `web/node_modules` or the generated bundle.
5. Add a context-specific CSS size only if one of the sizes above does not fit. The shell's icon rules are in its final `<style>` block; React view rules are in `web/src/alexandria.css` or `web/src/tl1.css`.

`src/ai_work_archive/ui.py` is the source of truth for the page shell. `macos/build-app.sh` copies it to `internal/archive/assets/ui.py` for the Go service. If working without the app build, copy that file explicitly. Rebuild the React bundle after changes in `web/src`:

```sh
cp src/ai_work_archive/ui.py internal/archive/assets/ui.py
npm --prefix web run build
npm --prefix web run check-bundle
(cd web && npx tsc --noEmit)
go test ./...
```

Inspect the new icon in the app at its final size and verify that the symbol resolves in both the Python and Go-served page shells.
