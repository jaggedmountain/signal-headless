#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Renders an ANSI terminal capture (tmux capture-pane -e -p) to a PNG, with
colour emoji: a monospace font for text, Noto Color Emoji for wide
characters. Used by build/screenshot.sh.

    build/screenshot.py CAPTURE.ans OUT.png
"""
import re
import sys
import unicodedata

from PIL import Image, ImageDraw, ImageFont

MONO = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
MONO_BOLD = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"
EMOJI = "/usr/share/fonts/truetype/noto/NotoColorEmoji.ttf"  # bitmap font: size 109 only
SIZE = 26
LINE = 1.45
PAD, BAR, RADIUS = 36, 44, 14

BG = (23, 23, 23)
FG = (212, 212, 212)
PALETTE = [  # ANSI 0-15
    (0, 0, 0), (205, 49, 49), (13, 188, 121), (229, 229, 16),
    (86, 137, 234), (188, 63, 188), (17, 168, 205), (229, 229, 229),
    (118, 118, 118), (241, 76, 76), (35, 209, 139), (245, 245, 67),
    (132, 170, 255), (214, 112, 214), (41, 184, 219), (245, 245, 245),
]


def xterm256(n):
    if n < 16:
        return PALETTE[n]
    if n < 232:
        n -= 16
        steps = [0, 95, 135, 175, 215, 255]
        return (steps[n // 36], steps[n // 6 % 6], steps[n % 6])
    v = 8 + (n - 232) * 10
    return (v, v, v)


def wide(ch):
    return unicodedata.east_asian_width(ch) in ("W", "F")


def parse(line):
    """Yields (char, fg, bg, bold) per character; SGR only."""
    fg = bg = None
    bold = rev = False
    for tok in re.split(r"(\x1b\[[0-9;]*m)", line):
        if tok.startswith("\x1b["):
            codes = [int(c) if c else 0 for c in tok[2:-1].split(";")] or [0]
            i = 0
            while i < len(codes):
                c = codes[i]
                if c == 0:
                    fg = bg = None
                    bold = rev = False
                elif c == 1:
                    bold = True
                elif c == 22:
                    bold = False
                elif c == 7:
                    rev = True
                elif c == 27:
                    rev = False
                elif 30 <= c <= 37:
                    fg = PALETTE[c - 30]
                elif 90 <= c <= 97:
                    fg = PALETTE[c - 90 + 8]
                elif c == 39:
                    fg = None
                elif 40 <= c <= 47:
                    bg = PALETTE[c - 40]
                elif 100 <= c <= 107:
                    bg = PALETTE[c - 100 + 8]
                elif c == 49:
                    bg = None
                elif c in (38, 48) and i + 1 < len(codes):
                    if codes[i + 1] == 5:
                        col, i = xterm256(codes[i + 2]), i + 2
                    else:
                        col, i = tuple(codes[i + 2:i + 5]), i + 4
                    if c == 38:
                        fg = col
                    else:
                        bg = col
                i += 1
            continue
        for ch in tok:
            f, b = fg or FG, bg
            if rev:
                f, b = (bg or BG), (fg or FG)
            yield ch, f, b, bold


def main(src, out):
    lines = open(src, encoding="utf-8").read().rstrip("\n").split("\n")
    mono = ImageFont.truetype(MONO, SIZE)
    bold = ImageFont.truetype(MONO_BOLD, SIZE)
    emoji = ImageFont.truetype(EMOJI, 109)
    cw = mono.getlength("M")
    lh = round(SIZE * LINE)
    cols = max(sum(2 if wide(c) else 1 for c, *_ in parse(l)) for l in lines)
    w = round(PAD * 2 + cols * cw)
    h = PAD * 2 + BAR + len(lines) * lh
    img = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.rounded_rectangle((0, 0, w - 1, h - 1), RADIUS, fill=BG)
    for i, col in enumerate([(255, 95, 86), (255, 189, 46), (39, 201, 63)]):
        cx, cy = PAD + i * 26, PAD // 2 + 10
        d.ellipse((cx - 8, cy - 8, cx + 8, cy + 8), fill=col)
    for row, line in enumerate(lines):
        y = PAD + BAR + row * lh
        x = 0
        for ch, fg, bg, isbold in parse(line):
            n = 2 if wide(ch) else 1
            px = PAD + x * cw
            if bg:
                d.rectangle((px, y, px + n * cw, y + lh), fill=bg)
            if ch in "│─":
                # Box lines span the whole cell, so they join across the
                # taller-than-glyph line spacing.
                if ch == "│":
                    d.line((px + cw / 2, y, px + cw / 2, y + lh), fill=fg, width=2)
                else:
                    d.line((px, y + lh / 2, px + cw, y + lh / 2), fill=fg, width=2)
            elif ch != " ":
                if n == 2 and ord(ch) > 0x2000:
                    g = Image.new("RGBA", (136, 128), (0, 0, 0, 0))
                    ImageDraw.Draw(g).text((0, 0), ch, font=emoji, embedded_color=True)
                    g = g.crop(g.getbbox() or (0, 0, 1, 1))
                    side = round(SIZE * 1.1)
                    g.thumbnail((side, side), Image.LANCZOS)
                    img.alpha_composite(g, (round(px + (2 * cw - g.width) / 2), round(y + (lh - g.height) / 2)))
                else:
                    d.text((px, y + (lh - SIZE) / 2 - 2), ch, font=bold if isbold else mono, fill=fg)
            x += n
    img.save(out)


if __name__ == "__main__":
    main(*sys.argv[1:3])
