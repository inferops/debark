# README artwork

Artwork for the repository [README](../../README.md). All of it is self-contained:
the SVGs carry no external fonts, styles, or scripts, and the screenshot is a
plain image file.

| File | Used for |
| --- | --- |
| `logo-mark-light.svg`, `logo-mark-dark.svg` | The README masthead |
| `how-it-works-light.svg`, `how-it-works-dark.svg` | The three-step workflow diagram |
| `desktop-packages.webp` | The desktop app section |

The camel logos are copied from the Debark website repository,
`debark-site/public/logo-mark.svg` (dark backgrounds) and
`debark-site/public/logo-mark-light.svg` (light backgrounds).

The workflow diagram simplifies the website's `AirGapFlowDiagram.astro` for the
repository README: three cards left to right, one per numbered step, each
labelled with the machine it runs on. It uses the same warm neutral and amber
palette as the website. Keep the text and geometry identical in the light and
dark variants when editing either SVG, and keep both in sync with the numbered
steps in the README.

The README uses `<picture>` to select the dark variant of each pair, with the
light variant as the fallback. Keep the alt text in sync with the diagram.

The screenshot is cropped from the website repository's
`public/images/screenshots/desktop-packages-full.webp`, taking the top-left
1060×553 region. The crop is deliberate: the full capture predates the rename
to Debark and still shows the old product name in the selection panel and the
status bar. Recapture the desktop app before using a wider crop here.
