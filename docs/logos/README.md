# Brewlet — Logo

The official **Brewlet** logo: a coffee bean paired with the `brew`**`let`** wordmark.
It captures the project in one mark — *run Java applications (☕) on Kubernetes, ship just your app.*

![Brewlet logo](./brewlet-logo.svg)

| File | Description |
|------|-------------|
| [`brewlet-logo.svg`](./brewlet-logo.svg) | Horizontal lockup — coffee-bean mark + `brew`**`let`** wordmark (brown → Kubernetes blue) and tagline. |

## Palette

| Token | Hex | Use |
|-------|-----|-----|
| Coffee brown | `#6F4E37` | `brew`, bean, primary text |
| Kubernetes blue | `#326CE5` | `let`, accent |
| Cream | `#F5E6C8` | bean groove, highlights |

## Rendering

The SVG is self-contained (system sans-serif fallback). To rasterize:

```bash
rsvg-convert -w 720 brewlet-logo.svg -o brewlet-logo.png
# or
inkscape brewlet-logo.svg --export-type=png -w 720
```
