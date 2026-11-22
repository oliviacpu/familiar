# /// script
# dependencies = ["resvg-py", "pillow"]
# ///
"""Render menubar.png, tray.ico and tray.png from familiar.svg: uv run icons/render.py"""

import io
from pathlib import Path

import resvg_py
from PIL import Image

icons = Path(__file__).parent
svg = (icons / "familiar.svg").read_text()


def render(size, fill):
    png = resvg_py.svg_to_bytes(svg_string=svg.replace("#000000", fill), width=size, height=size)
    return Image.open(io.BytesIO(bytes(png))).convert("RGBA")


# macOS: systray draws the menu bar image at 16x16 pt, so 32 px is exact on Retina.
render(32, "#000000").save(icons / "menubar.png", optimize=True)

# Windows: one render per size rather than scaling one down.
sizes = [16, 20, 24, 32, 48, 64]
frames = [render(s, "#8B5CF6") for s in sizes]
frames[-1].save(icons / "tray.ico", sizes=[(s, s) for s in sizes], append_images=frames[:-1])
# Linux: the panel takes one image and scales it, so 48 px halves cleanly to 24.
render(48, "#8B5CF6").save(icons / "tray.png", optimize=True)
