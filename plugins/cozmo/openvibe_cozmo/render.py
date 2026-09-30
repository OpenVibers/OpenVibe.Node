"""Rendering for Cozmo's face screen.

PyCozmo's ``Client.display_image`` takes a 128x32 image in mode "1" (its ImageEncoder rejects anything else); the robot
doubles it vertically on its 128x64 OLED. Everything here is pure Pillow so it can be tested without a robot.
"""

import base64
import io

from PIL import Image, ImageDraw, ImageFont, ImageOps

WIDTH = 128
HEIGHT = 32
MAX_PNG_B64 = 256 * 1024  # a 128x32 picture never needs more; refuse bigger payloads before decoding

FACES = ("neutral", "happy", "sad", "surprised", "sleepy", "angry")


def blank():
    return Image.new("1", (WIDTH, HEIGHT), 0)


def _font():
    try:
        return ImageFont.load_default(size=11)  # Pillow >= 10.1 (FreeType build)
    except TypeError:
        return ImageFont.load_default()


def _wrap(draw, text, font, width):
    """Greedy word wrap by pixel width. Words longer than a line are cut by characters."""
    lines = []
    for para in text.splitlines() or [""]:
        line = ""
        for word in para.split():
            cand = word if not line else line + " " + word
            if draw.textlength(cand, font=font) <= width:
                line = cand
                continue
            if line:
                lines.append(line)
            while draw.textlength(word, font=font) > width and len(word) > 1:
                cut = len(word)
                while cut > 1 and draw.textlength(word[:cut], font=font) > width:
                    cut -= 1
                lines.append(word[:cut])
                word = word[cut:]
            line = word
        lines.append(line)
    return lines


def render_text(text):
    """Centered text, wrapped to as many lines as fit; the rest is cut with an ellipsis."""
    img = blank()
    draw = ImageDraw.Draw(img)
    font = _font()
    text = " ".join(str(text).split()) if "\n" not in str(text) else str(text)
    lines = _wrap(draw, text, font, WIDTH - 2)
    box = draw.textbbox((0, 0), "Ag", font=font)
    line_h = max(1, box[3] - box[1] + 1)
    max_lines = max(1, HEIGHT // line_h)
    if len(lines) > max_lines:
        lines = lines[:max_lines]
        last = lines[-1]
        while last and draw.textlength(last + "...", font=font) > WIDTH - 2:
            last = last[:-1]
        lines[-1] = last + "..."
    top = (HEIGHT - line_h * len(lines)) // 2 - box[1]
    for i, line in enumerate(lines):
        w = draw.textlength(line, font=font)
        draw.text(((WIDTH - w) // 2, top + i * line_h), line, fill=1, font=font)
    return img


def render_face(name):
    """Two simple eyes. Unknown names raise ValueError."""
    name = str(name).lower()
    if name not in FACES:
        raise ValueError("unknown face %r (known: %s)" % (name, ", ".join(FACES)))
    img = blank()
    d = ImageDraw.Draw(img)
    for cx in (40, 88):
        x0, x1 = cx - 14, cx + 14
        if name == "neutral":
            d.rounded_rectangle((x0, 6, x1, 26), radius=5, fill=1)
        elif name == "happy":
            # Upward arcs (closed, smiling eyes).
            d.pieslice((x0, 8, x1, 36), 180, 360, fill=1)
            d.pieslice((x0 + 5, 14, x1 - 5, 40), 180, 360, fill=0)
        elif name == "sad":
            # Eyes with the outer top corner cut away, drooping outwards.
            d.rounded_rectangle((x0, 10, x1, 26), radius=4, fill=1)
            if cx < 64:
                d.polygon([(x0 - 1, 9), (x0 - 1, 18), (x1 + 1, 9)], fill=0)
            else:
                d.polygon([(x1 + 1, 9), (x1 + 1, 18), (x0 - 1, 9)], fill=0)
        elif name == "surprised":
            d.ellipse((cx - 12, 3, cx + 12, 29), fill=1)
            d.ellipse((cx - 5, 10, cx + 5, 22), fill=0)
        elif name == "sleepy":
            d.rectangle((x0, 19, x1, 23), fill=1)
        elif name == "angry":
            d.rounded_rectangle((x0, 8, x1, 26), radius=4, fill=1)
            if cx < 64:
                d.polygon([(x0 - 1, 7), (x1 + 1, 7), (x1 + 1, 16)], fill=0)
            else:
                d.polygon([(x0 - 1, 7), (x1 + 1, 7), (x0 - 1, 16)], fill=0)
    return img


def render_png_b64(data):
    """Decode a base64 image (PNG or anything Pillow reads), fit it into 128x32 keeping aspect, dither to 1 bit."""
    if not isinstance(data, str) or not data:
        raise ValueError("image_png_b64 must be a non-empty string")
    if len(data) > MAX_PNG_B64:
        raise ValueError("image too large (%d bytes of base64, max %d)" % (len(data), MAX_PNG_B64))
    try:
        raw = base64.b64decode(data, validate=False)
        src = Image.open(io.BytesIO(raw))  # lazy: reads the header only
        if src.width * src.height > 4096 * 4096:
            raise ValueError("image dimensions too large")
        src.load()
    except ValueError:
        raise
    except Exception as e:
        raise ValueError("not a readable image: %s" % e)
    grey = src.convert("L")
    fitted = ImageOps.pad(grey, (WIDTH, HEIGHT), color=0)
    return fitted.convert("1")
