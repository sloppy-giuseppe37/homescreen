# Design Assets

Prototyping files for the PWA icon. Uses [Lucide](https://lucide.dev) icons loaded from CDN.

## Files

| File | Purpose |
|---|---|
| `icon-preview.html` | Side-by-side comparison of icon layout variants. Add new layouts here to compare options. |
| `icon-render.html` | Export-ready render at 512×512. Screenshot this to produce the final PNGs. |

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

5. **Export PNGs** — once happy, transfer your final values to `icon-render.html` (which renders at 512×512), then:
   ```bash
   # Screenshot the render page at 512x512 for icon-512.png
   # Then downscale for icon-192.png:
   convert static/icons/icon-512.png -resize 192x192 static/icons/icon-192.png
   ```
   Or use the browser tool's element screenshot on `#icon`.

6. **Rebuild** — the icons are embedded in the binary:
   ```bash
   go build -o homescreen . && sudo systemctl restart homescreen
   ```
