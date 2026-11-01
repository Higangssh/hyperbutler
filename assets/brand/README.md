# hyperbutler brand assets

| File | Use |
|---|---|
| `logo-light.png`, `logo-dark.png` | Wordmark with the mascot, transparent, for light and dark backgrounds (the README switches with `<picture>`) |
| `mascot.png` | The butler robot alone, transparent |
| `mascot-480.png` | `mascot.png` scaled to 480 px wide (bicubic), embedded in the GUI: a quarter of the bytes, and enough for its largest use (230 CSS px at 200%) |
| `icon-source.png` | Flat app icon (stacked layers with a bolt), 512 px, transparent |
| `icon-16.png` to `icon-256.png` | The app icon at fixed sizes, for the executable icon, favicon and a future GUI |
| `icon-dark.png` | The app icon on a dark tile |
| `icon-monochrome.png` | One-colour icon, for places that tint it |
| `preview-light.png` | 1600x650 banner, for the GitHub social preview |

`logo-dark.png` has the first stroke of the "h" recoloured to match the rest of "hyper";
it came out navy in the delivered file. The dark banner was left out for the same reason:
on its navy background the stroke can't be told apart by colour, so it needs a new export.
