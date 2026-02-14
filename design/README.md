# Design Assets

Prototyping files for the PWA icon. Uses [Lucide](https://lucide.dev) icons loaded from CDN.

## Files

| File | Purpose |
|---|---|
| `icon.svg` | Source SVG for the current PWA icon. Edit this to change the icon. |
| `icon-preview.html` | Side-by-side comparison of icon layout variants (uses Lucide CDN). Add new layouts here to compare options. |
| `icon-render.html` | Browser-based render at 512×512 for visual preview. |

## How to iterate on the icon

1. **Serve the files** (from the repo root):
   ```bash
   busybox httpd -f -p 8001 -h design/
   ```
   Then open `http://localhost:8001/icon-preview.html`.

2. **Try new layouts** — duplicate a block in `icon-preview.html`, add a new CSS layout class, and adjust icon positions/sizes.

3. **Change icons** — swap `data-lucide="fan"` etc. for any icon name from the [Lucide icon list](https://lucide.dev/icons/). The vendored icon font in `static/lucide.css` has the full set.

4. **Change the gradient** — edit the `background` property. The current gradient uses `oklch` color interpolation for vibrant midtones:
   ```css
   background: linear-gradient(to top left in oklch, #f4a942, #2d6ab8);
   ```

5. **Export PNGs** — once happy, update `icon.svg` with your final layout, then:
   ```bash
   rsvg-convert -w 512 -h 512 design/icon.svg -o static/icons/icon-512.png
   rsvg-convert -w 192 -h 192 design/icon.svg -o static/icons/icon-192.png
   ```
   This produces PNGs with transparent backgrounds. Requires `librsvg2-bin` (`sudo apt install librsvg2-bin`).

   The SVG gradient approximates an oklch blue→orange gradient with 5 stops since SVG doesn't support oklch natively.

6. **Rebuild** — the icons are embedded in the binary:
   ```bash
   go build -o homescreen . && sudo systemctl restart homescreen
   ```
