"""
Generates the macOS DMG window background.

The DMG deliberately ships NO /Applications shortcut: a machine-wide install
needs an admin prompt and cannot be replaced silently by the auto-updater, so
the app installs itself into ~/Applications the first time it is launched (see
relocate_darwin.go). The background therefore tells the user to double-click
the app rather than to drag it, and must not draw an arrow to a drop target
that no longer exists.

One image ships on every channel, and dev/stg builds are deliberately NOT
silent-update capable (silent_update_capable: "false" in release-nonprod.yml),
so the copy must not promise automatic updates.

    python aiexpedite-local-terminal/scripts/generate-dmg-background.py

Creates:
  aiexpedite-local-terminal/assets/dmg-background.png

Keep the size in sync with `window_rect` and the icon position in sync with
`icon_locations` in the "Package as DMG" step of .github/workflows/
release-prod.yml and release-nonprod.yml.
"""

from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
OUT_PNG = ROOT / "assets" / "dmg-background.png"

WIDTH, HEIGHT = 600, 400
BACKGROUND = (247, 247, 247, 255)
HEADLINE = (74, 74, 74, 255)
SUBTEXT = (138, 138, 138, 255)

# Matches icon_locations = {appname: (300, 190)} with icon_size = 100, so the
# text starts below the icon and its Finder filename label.
ICON_CENTER_Y = 190
ICON_SIZE = 100

HEADLINE_TEXT = "Double-click to install"
SUBTEXT_TEXT = "It copies itself to your Applications folder \u2014 nothing to drag."

FONT_CANDIDATES = [
    "/System/Library/Fonts/HelveticaNeue.ttc",
    "/System/Library/Fonts/Helvetica.ttc",
    "/System/Library/Fonts/SFNS.ttf",
]


def load_font(size: int) -> ImageFont.FreeTypeFont:
    for path in FONT_CANDIDATES:
        if Path(path).exists():
            return ImageFont.truetype(path, size)
    return ImageFont.load_default(size)


def draw_centered(draw: ImageDraw.ImageDraw, y: int, text: str, font, fill) -> None:
    draw.text((WIDTH // 2, y), text, font=font, fill=fill, anchor="mm")


def main() -> None:
    image = Image.new("RGBA", (WIDTH, HEIGHT), BACKGROUND)
    draw = ImageDraw.Draw(image)

    # Icon bottom + ~24px for the filename Finder draws under it.
    first_free_y = ICON_CENTER_Y + ICON_SIZE // 2 + 24

    draw_centered(draw, first_free_y + 46, HEADLINE_TEXT, load_font(20), HEADLINE)
    draw_centered(draw, first_free_y + 76, SUBTEXT_TEXT, load_font(12), SUBTEXT)

    OUT_PNG.parent.mkdir(parents=True, exist_ok=True)
    image.save(OUT_PNG)
    print(f"Wrote {OUT_PNG}")


if __name__ == "__main__":
    main()
