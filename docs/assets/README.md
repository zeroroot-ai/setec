<!-- SPDX-License-Identifier: Apache-2.0 -->
# Brand Assets

Canonical source is [`logo.svg`](./logo.svg). All raster outputs in this directory are derived from it and can be regenerated with the commands below.

## Files

| File                  | Size       | Used for                                    |
|-----------------------|------------|---------------------------------------------|
| `logo.svg`            | 256x256 vb | Canonical, hand-authored logo               |
| `logo-32.png`         | 32x32      | Small UI contexts, favicon source           |
| `logo-128.png`        | 128x128    | README header, docs site                    |
| `logo-512.png`        | 512x512    | Presentations, high-density renders         |
| `favicon.ico`         | 32x32 ICO  | Docs/marketing sites                        |
| `social-preview.png`  | 1280x640   | GitHub repository social preview            |

## Regenerating the raster outputs

All raster files are produced from `logo.svg` using ImageMagick. `rsvg-convert` (from librsvg) also works and produces cleaner output if you have it installed. The commands below assume ImageMagick 6 or 7 is on `PATH` (`convert` or `magick`).

```bash
cd docs/assets

# PNG renders at 32/128/512.
convert -background none logo.svg -resize 32x32  logo-32.png
convert -background none logo.svg -resize 128x128 logo-128.png
convert -background none logo.svg -resize 512x512 logo-512.png

# Favicon derived from the smallest PNG.
convert logo-32.png favicon.ico

# Social preview: logo on the left, wordmark and tagline to the right.
convert -size 1280x640 xc:"#FAFAFA" \
  \( logo.svg -resize 400x400 \) -gravity West -geometry +120+0 -composite \
  -font DejaVu-Sans-Bold -pointsize 56 -fill "#101216" -gravity West -annotate +560+-40 "Setec" \
  -font DejaVu-Sans -pointsize 28 -fill "#101216" -gravity West -annotate +560+40 "microVM isolation as a Kubernetes primitive" \
  social-preview.png
```

If `rsvg-convert` is installed you can use it for the PNG steps:

```bash
rsvg-convert -w 32  -h 32  logo.svg -o logo-32.png
rsvg-convert -w 128 -h 128 logo.svg -o logo-128.png
rsvg-convert -w 512 -h 512 logo.svg -o logo-512.png
```

## Design notes

The mark is a geometric uppercase "S" assembled from two interlocking bracket strokes. The brackets face each other, and the notch where they meet carries the only accent colour in the palette. The mark deliberately avoids literal references to the 1992 film that inspired the project name; the containment motif reads on its own.

Palette:

- `#101216` primary ink (strokes, wordmark)
- `#F25C54` accent (notch only)
- `#FAFAFA` paper (background chip)
