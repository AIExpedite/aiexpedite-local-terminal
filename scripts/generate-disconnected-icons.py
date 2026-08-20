"""
Generates the overlay variants of the tray / app icons from the existing logo
assets.

Two overlays, two meanings — deliberately different shapes so they are never
confused at 16x16:

  * "disconnected" - a red prohibition sign (ring + diagonal bar) centred over
    the whole logo. A bottom-right dot used to be used here, but it reads as a
    notification badge ("you have a message"), not as "this agent is offline".
  * "pending"      - a small red dot in the TOP-RIGHT corner, the conventional
    unread/pending-message badge position. Nothing embeds these yet; they are
    generated so the pending state is ready to wire up.

Run with:

    python aiexpedite-local-terminal/scripts/generate-disconnected-icons.py

Creates:
  aiexpedite-local-terminal/assets/icon-disconnected.png
  aiexpedite-local-terminal/assets/aiexpedite-tray-icon-disconnected.ico
  aiexpedite-local-terminal/assets/icon-pending.png
  aiexpedite-local-terminal/assets/aiexpedite-tray-icon-pending.ico

Note: icon-disconnected.png is NOT a macOS template icon — the red overlay must
stay red, so it is consumed via systray.SetIcon, not SetTemplateIcon.
"""

from __future__ import annotations

import math
from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent
ASSETS = ROOT / "assets"

# macOS / Linux tray silhouette, and the full-colour logo the Windows ICO is
# built from (using the logo rather than the ICO avoids re-reading ICO frames).
SRC_PNG = ASSETS / "icon.png"
SRC_LOGO = ASSETS / "aiexpedite-logo-256.png"

OUT_PNG_DISCONNECTED = ASSETS / "icon-disconnected.png"
OUT_ICO_DISCONNECTED = ASSETS / "aiexpedite-tray-icon-disconnected.ico"
OUT_PNG_PENDING = ASSETS / "icon-pending.png"
OUT_ICO_PENDING = ASSETS / "aiexpedite-tray-icon-pending.ico"

PNG_SIZE = 64
ICO_SIZES = (16, 32, 48, 64, 128, 256)

RED = (229, 57, 53, 255)
WHITE = (255, 255, 255, 255)

# Pillow's draw primitives are not antialiased, so every overlay is drawn on a
# canvas this many times larger and scaled back down — otherwise the ring and
# bar are visibly jagged at 16x16.
SUPERSAMPLE = 8


def _draw_overlay(img: Image.Image, paint) -> Image.Image:
    """Run `paint` on a supersampled transparent canvas, then composite it."""
    img = img.convert("RGBA")
    w, h = img.size
    big = Image.new("RGBA", (w * SUPERSAMPLE, h * SUPERSAMPLE), (0, 0, 0, 0))
    paint(ImageDraw.Draw(big), w * SUPERSAMPLE, h * SUPERSAMPLE)
    return Image.alpha_composite(img, big.resize((w, h), Image.LANCZOS))


def add_disabled_overlay(img: Image.Image) -> Image.Image:
    """Composite a red prohibition sign (ring + 45 degree bar) over the icon.

    Each stroke is drawn over a slightly wider white copy of itself so the sign
    stays legible on both the black macOS silhouette and the full-colour
    Windows logo.
    """

    def paint(draw: ImageDraw.ImageDraw, w: int, h: int) -> None:
        size = min(w, h)
        stroke = max(2 * SUPERSAMPLE, round(size * 0.12))
        halo = stroke + max(1, round(size * 0.035)) * 2
        cx, cy = w / 2, h / 2
        radius = size / 2
        # Pillow draws an ellipse outline INWARD from its bounding box, so the
        # halo ring spans [radius - halo, radius] and the red ring is centred
        # inside it.
        halo_box = (cx - radius, cy - radius, cx + radius, cy + radius)
        inset = (halo - stroke) / 2
        red_box = (
            cx - radius + inset,
            cy - radius + inset,
            cx + radius - inset,
            cy + radius - inset,
        )
        # Bar runs top-left -> bottom-right at 45 degrees, ending on the red
        # ring's centre line (line widths ARE centred on the line, unlike
        # ellipse outlines).
        d = (radius - halo / 2) * math.sqrt(0.5)
        bar = ((cx - d, cy - d), (cx + d, cy + d))

        # White halo first, red on top.
        draw.ellipse(halo_box, outline=WHITE, width=halo)
        draw.line(bar, fill=WHITE, width=halo)
        draw.ellipse(red_box, outline=RED, width=stroke)
        draw.line(bar, fill=RED, width=stroke)

    return _draw_overlay(img, paint)


def add_red_dot(img: Image.Image) -> Image.Image:
    """Composite a small red circle with a white ring in the TOP-RIGHT corner.

    This is the pending-message badge, not the disconnected marker — see the
    module docstring.
    """

    def paint(draw: ImageDraw.ImageDraw, w: int, h: int) -> None:
        # Dot is ~22% of the shortest side, pinned to the top-right.
        radius = max(int(min(w, h) * 0.22), 4 * SUPERSAMPLE)
        margin = max(int(min(w, h) * 0.04), SUPERSAMPLE)
        cx = w - radius - margin
        cy = radius + margin
        # White outline first so the red dot pops on light backgrounds.
        outer = radius + max(radius // 5, 1)
        draw.ellipse((cx - outer, cy - outer, cx + outer, cy + outer), fill=WHITE)
        draw.ellipse((cx - radius, cy - radius, cx + radius, cy + radius), fill=RED)

    return _draw_overlay(img, paint)


def _resize(img: Image.Image, size: int) -> Image.Image:
    return img.convert("RGBA").resize((size, size), Image.LANCZOS)


def build_png(overlay, out: Path) -> None:
    src = Image.open(SRC_PNG)
    overlay(_resize(src, PNG_SIZE)).save(out, format="PNG")
    print(f"wrote {out}")


def build_ico(overlay, out: Path) -> None:
    """Stamp the overlay at every tray size, then pack one multi-size ICO.

    Stamping per size (rather than stamping once and letting Pillow downscale)
    keeps the stroke weights proportional and the edges crisp at 16x16.
    """
    src = Image.open(SRC_LOGO)
    frames = [overlay(_resize(src, s)) for s in ICO_SIZES]
    # Largest frame is the base; the rest ride along as additional ICO entries.
    frames[-1].save(
        out,
        format="ICO",
        sizes=[(s, s) for s in ICO_SIZES],
        append_images=frames[:-1],
    )
    print(f"wrote {out}")


def main() -> None:
    for src in (SRC_PNG, SRC_LOGO):
        if not src.exists():
            raise SystemExit(f"Missing source image: {src}")

    build_png(add_disabled_overlay, OUT_PNG_DISCONNECTED)
    build_ico(add_disabled_overlay, OUT_ICO_DISCONNECTED)

    # Kept ready for the pending-message state; nothing embeds these yet.
    build_png(add_red_dot, OUT_PNG_PENDING)
    build_ico(add_red_dot, OUT_ICO_PENDING)


if __name__ == "__main__":
    main()
