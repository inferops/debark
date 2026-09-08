# README artwork

The camel logos are copied from the Debark website repository,
`debark-site/public/logo-mark.svg` (dark backgrounds) and
`debark-site/public/logo-mark-light.svg` (light backgrounds).

The workflow diagram simplifies the website's `AirGapFlowDiagram.astro` for
the repository README. It uses the same warm neutral and amber palette, with a
vertical layout for narrow screens. Keep the text and geometry identical in
the light and dark variants when editing either SVG.

All artwork is self-contained SVG with no external fonts, styles, or scripts.
The README uses `<picture>` to select the dark variant, with the light variant
as the fallback. Keep its alt text and numbered steps in sync with the diagram.
