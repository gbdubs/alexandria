# Logo concepts

Six exploratory directions for the app icon and header mark, drawn while the
app was still called Alexandria. **Pharos was chosen** and the app took its
name: the shipping icon source is [`macos/AppIcon.svg`](../../../macos/AppIcon.svg)
(rebuild `AppIcon.icns` with `macos/make-icon.sh`), and the header mark is
inlined in `src/ai_work_archive/ui.py`. Open
[`preview.html`](preview.html) in a browser to see every concept at Dock and
Finder sizes, plus the header lockup in light and dark themes.

Each concept has two files:

- `<name>-icon.svg`: a 1024×1024 macOS app icon on Apple's grid (824-pt
  continuous-corner squircle, baked drop shadow, glass rim). It can be rasterised
  directly into an `.iconset` for `iconutil`.
- `<name>-mark.svg`: a 48×48 flat mark for the app header. Its fills use the
  UI tokens (`--accent`, `--gold`, `--panel`, `--muted`, `--line`), with
  light-theme fallbacks. When inlined into `ui.py`, it follows the light and
  dark themes automatically.

| # | Concept | Idea |
| --- | --- | --- |
| 1 | **Pharos** | The lighthouse of Alexandria. Its beam lights up rows of transcript, with one match glowing gold. It stands for search, and it ties directly to the name. |
| 2 | **Armarium** | Roman scroll cubbies. Each scroll hangs a *sillybos* title tag (i.e. metadata), and one scroll is lit: the hit. |
| 3 | **Card Catalog** | The library's original search index: a drawer of cards with the matching card pulled up and highlighted. |
| 4 | **Bookmarked Thread** | Stacked conversation bubbles with a ribbon bookmark: saved agent conversations. It holds up best at 16 px. |
| 5 | **Shelf Chart** | Book spines rising like a bar chart on graph paper: a library you can analyse. |
| 6 | **Gilded Volume** | An evolution of today's "A" book mark: green leather, a gold-tooled A, and a ribbon. |

The palette matches `src/ai_work_archive/ui.py`: accent `#315845`, deep accent
`#203f31`, gold `#ad823b`, panel `#fffaf0`, and background `#eee7d8`.
Since Pharos shipped, the app header and dark theme take their colors from the
icon itself: harbor green (`#2b5241` to `#10241b`), stone `#f3ebdc`, brass
`#d4a857`, and lamp `#ffd978`.

The Gilded Volume "A" is outlined from Charter Bold, the app's display serif.
Confirm the font licence, or redraw the letterform, before shipping that concept.
