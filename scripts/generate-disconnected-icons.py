"""
Generates the "disconnected" variants of the tray / app icons by loading
the existing icon PNG and compositing a small red circle in the bottom-right
corner. Run once during the terminal-disconnect-and-status-sync feature work:

    python aiexpedite-local-terminal/scripts/generate-disconnected-icons.py

Creates:
  aiexpedite-local-terminal/assets/icon-disconnected.png
  aiexpedite-local-terminal/assets/aiexpedite-tray-icon-disconnected.ico
"""

from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent
ASSETS = ROOT / "assets"

SRC_PNG = ASSETS / "icon.png"
SRC_ICO = ASSETS / "aiexpedite-tray-icon.ico"

OUT_PNG = ASSETS / "icon-disconnected.png"
OUT_ICO = ASSETS / "aiexpedite-tray-icon-disconnected.ico"

RED = (220, 53, 69, 255)  # bootstrap-ish red
WHITE_RING = (255, 255, 255, 255)


def add_red_dot(img: Image.Image) -> Image.Image:
    """Composite a small red circle with a white ring in the bottom-right."""
    img = img.convert("RGBA")
    w, h = img.size
    # Dot is ~35% of the shortest side, pinned to the bottom-right.
    radius = max(int(min(w, h) * 0.22), 4)
    margin = max(int(min(w, h) * 0.04), 1)
    cx = w - radius - margin
    cy = h - radius - margin

    overlay = Image.new("RGBA", img.size, (0, 0, 0, 0))
    draw = ImageDraw.Draw(overlay)
    # White outline first so the red dot pops on light backgrounds.
    outer = radius + max(radius // 5, 1)
    draw.ellipse(
        (cx - outer, cy - outer, cx + outer, cy + outer),
        fill=WHITE_RING,
    )
    draw.ellipse(
        (cx - radius, cy - radius, cx + radius, cy + radius),
        fill=RED,
    )
    return Image.alpha_composite(img, overlay)


def main() -> None:
    if not SRC_PNG.exists():
        raise SystemExit(f"Missing source PNG: {SRC_PNG}")

    # PNG variant (used by macOS + Linux tray)
    png = add_red_dot(Image.open(SRC_PNG))
    png.save(OUT_PNG, format="PNG")
    print(f"wrote {OUT_PNG}")

    # ICO variant (used by Windows tray). If the source .ico is missing or
    # unreadable, fall back to the PNG and let Pillow generate the ICO from
    # scratch in common sizes.
    try:
        src_ico = Image.open(SRC_ICO) if SRC_ICO.exists() else png
        base = src_ico.convert("RGBA") if src_ico.mode != "RGBA" else src_ico
    except Exception as exc:
        print(f"warn: could not load {SRC_ICO} ({exc}), falling back to PNG")
        base = png

    ico = add_red_dot(base)
    # Standard Windows tray/icon sizes.
    ico.save(
        OUT_ICO,
        format="ICO",
        sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)],
    )
    print(f"wrote {OUT_ICO}")


if __name__ == "__main__":
    main()
