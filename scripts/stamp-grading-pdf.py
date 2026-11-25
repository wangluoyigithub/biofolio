#!/usr/bin/env python3
"""Post-stamp any single-section A4 PDF with Biofolio machine-readable grading marks.

Stamps on every page (preserving 100% vector text & graphics on macOS via PDFKit/CoreGraphics):
  - Four 4mm x 4mm solid black corner registration anchors (6mm inset from A4 edges; centers form 194x281mm ref frame)
  - Footer page number ("第 X 页 / 共 Y 页") at bottom-center
  - Footer single-line code ("BF:<paper_id>:<page_no>") + 15mm x 15mm ISO 18004 vector QR (EC Level Q) at x=180mm, bottom=5mm
    (15mm logical -> ~13.1mm physical after 87.5% shrink onto 260x368mm 正度 8K, meeting Fuji 13mm @ 300 DPI spec)
  - Optional --update-pack <json> extracts single BFGRID0 anchor from the 5mm Title Row to recompute 180mm 10-col
    (18mm x 10mm) objectiveAnswerGrid & answer_box coordinates; blocks with non-zero exit if BFGRID0 is missing!

Usage:
  python3 scripts/stamp-grading-pdf.py input.pdf output.pdf --paper-id 7A07C84604 [--update-pack pack.json]
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import tempfile
from pathlib import Path

ALNUM = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"
EC_BITS = {"L": 1, "M": 0, "Q": 3, "H": 2}
RS_TABLE_Q = [
    None,
    (13, 1, 13, 0, 0),
    (22, 1, 22, 0, 0),
    (18, 2, 17, 0, 0),
    (26, 2, 24, 0, 0),
    (18, 2, 15, 2, 16),
    (24, 4, 19, 0, 0),
]
ALIGN = [None, [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34]]

GF_EXP = [0] * 512
GF_LOG = [0] * 256
_x = 1
for _i in range(255):
    GF_EXP[_i] = _x
    GF_EXP[_i + 255] = _x
    GF_LOG[_x] = _i
    _x <<= 1
    if _x & 0x100:
        _x ^= 0x11D


def _gf_mul(a: int, b: int) -> int:
    return GF_EXP[GF_LOG[a] + GF_LOG[b]] if (a and b) else 0


def _rs_remainder(data: list[int], ec_count: int) -> list[int]:
    poly = [1]
    for i in range(ec_count):
        nxt = [0] * (len(poly) + 1)
        factor = GF_EXP[i]
        for j, pv in enumerate(poly):
            nxt[j] ^= pv
            nxt[j + 1] ^= _gf_mul(pv, factor)
        poly = nxt
    rem = [0] * ec_count
    for b in data:
        factor = b ^ rem[0]
        rem = rem[1:] + [0]
        if factor:
            for j in range(ec_count):
                rem[j] ^= _gf_mul(poly[j + 1], factor)
    return rem


def _encode_bits(text: str) -> list[int]:
    bits: list[int] = []

    def push(val: int, length: int) -> None:
        for i in range(length - 1, -1, -1):
            bits.append((val >> i) & 1)

    if text and all(ch in ALNUM for ch in text):
        push(0b0010, 4)
        push(len(text), 9)
        for i in range(0, len(text), 2):
            a = ALNUM.index(text[i])
            if i + 1 < len(text):
                push(a * 45 + ALNUM.index(text[i + 1]), 11)
            else:
                push(a, 6)
    else:
        raw = text.encode("utf-8")
        push(0b0100, 4)
        push(len(raw), 8)
        for b in raw:
            push(b, 8)
    return bits


def _mask_fn(mask: int, r: int, c: int) -> bool:
    if mask == 0:
        return (r + c) % 2 == 0
    if mask == 1:
        return r % 2 == 0
    if mask == 2:
        return c % 3 == 0
    if mask == 3:
        return (r + c) % 3 == 0
    if mask == 4:
        return ((r // 2) + (c // 3)) % 2 == 0
    if mask == 5:
        return ((r * c) % 2) + ((r * c) % 3) == 0
    if mask == 6:
        return (((r * c) % 2) + ((r * c) % 3)) % 2 == 0
    return (((r + c) % 2) + ((r * c) % 3)) % 2 == 0


def _penalty(grid: list[list[int]], size: int) -> int:
    score = 0
    for r in range(size):
        run = 1
        for c in range(1, size):
            if grid[r][c] == grid[r][c - 1]:
                run += 1
                if run == 5:
                    score += 3
                elif run > 5:
                    score += 1
            else:
                run = 1
    for c in range(size):
        run = 1
        for r in range(1, size):
            if grid[r][c] == grid[r - 1][c]:
                run += 1
                if run == 5:
                    score += 3
                elif run > 5:
                    score += 1
            else:
                run = 1
    for r in range(size - 1):
        for c in range(size - 1):
            v = grid[r][c]
            if v == grid[r][c + 1] == grid[r + 1][c] == grid[r + 1][c + 1]:
                score += 3
    pat_a = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0]
    pat_b = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1]
    for r in range(size):
        line = grid[r]
        for i in range(size - 10):
            seg = line[i : i + 11]
            if seg == pat_a or seg == pat_b:
                score += 40
    for c in range(size):
        col = [grid[r][c] for r in range(size)]
        for i in range(size - 10):
            seg = col[i : i + 11]
            if seg == pat_a or seg == pat_b:
                score += 40
    dark = sum(sum(row) for row in grid)
    ratio = abs((dark * 100.0) / (size * size) - 50.0)
    score += int(ratio // 5) * 10
    return score


def qr_matrix_q(text: str) -> list[list[int]]:
    bits = _encode_bits(text)
    version = 0
    for v in range(1, 7):
        _, g1b, g1d, g2b, g2d = RS_TABLE_Q[v]
        if len(bits) <= (g1b * g1d + g2b * g2d) * 8:
            version = v
            break
    if not version:
        raise ValueError(f"QR payload too long: {text}")

    ec_len, g1b, g1d, g2b, g2d = RS_TABLE_Q[version]
    total_data = g1b * g1d + g2b * g2d
    padded = list(bits)
    padded.extend([0] * min(4, total_data * 8 - len(padded)))
    while len(padded) % 8:
        padded.append(0)
    data = []
    for i in range(0, len(padded), 8):
        b = 0
        for j in range(8):
            b = (b << 1) | padded[i + j]
        data.append(b)
    pad_idx = 0
    while len(data) < total_data:
        data.append(0xEC if pad_idx % 2 == 0 else 0x11)
        pad_idx += 1

    blocks, ecs = [], []
    off = 0
    for _ in range(g1b):
        blk = data[off : off + g1d]
        off += g1d
        blocks.append(blk)
        ecs.append(_rs_remainder(blk, ec_len))
    for _ in range(g2b):
        blk = data[off : off + g2d]
        off += g2d
        blocks.append(blk)
        ecs.append(_rs_remainder(blk, ec_len))

    codewords = []
    for i in range(max(g1d, g2d)):
        for blk in blocks:
            if i < len(blk):
                codewords.append(blk[i])
    for i in range(ec_len):
        for ec in ecs:
            codewords.append(ec[i])

    size = 17 + 4 * version
    grid = [[0] * size for _ in range(size)]
    is_func = [[False] * size for _ in range(size)]

    def set_mod(r: int, c: int, dark: bool) -> None:
        if 0 <= r < size and 0 <= c < size:
            grid[r][c] = 1 if dark else 0
            is_func[r][c] = True

    def draw_finder(tr: int, tc: int) -> None:
        for dr in range(-1, 8):
            for dc in range(-1, 8):
                r, c = tr + dr, tc + dc
                if 0 <= r < size and 0 <= c < size:
                    dark = (
                        0 <= dr <= 6
                        and 0 <= dc <= 6
                        and (dr in (0, 6) or dc in (0, 6) or (2 <= dr <= 4 and 2 <= dc <= 4))
                    )
                    set_mod(r, c, dark)

    draw_finder(0, 0)
    draw_finder(0, size - 7)
    draw_finder(size - 7, 0)
    for i in range(8, size - 8):
        set_mod(6, i, i % 2 == 0)
        set_mod(i, 6, i % 2 == 0)
    for cr in ALIGN[version]:
        for cc in ALIGN[version]:
            if is_func[cr][cc]:
                continue
            for dr in range(-2, 3):
                for dc in range(-2, 3):
                    set_mod(cr + dr, cc + dc, max(abs(dr), abs(dc)) != 1)
    for i in range(9):
        if i != 6:
            set_mod(8, i, False)
            set_mod(i, 8, False)
    for i in range(size - 8, size):
        set_mod(8, i, False)
        set_mod(i, 8, False)
    set_mod(4 * version + 9, 8, True)

    unmasked = [row[:] for row in grid]
    bit_idx = 0
    total_bits = len(codewords) * 8
    upward = True
    col = size - 1
    while col >= 1:
        if col == 6:
            col = 5
        for step in range(size):
            row = (size - 1 - step) if upward else step
            for dx in range(2):
                c = col - dx
                if is_func[row][c]:
                    continue
                dark = 0
                if bit_idx < total_bits:
                    dark = (codewords[bit_idx >> 3] >> (7 - (bit_idx & 7))) & 1
                    bit_idx += 1
                unmasked[row][c] = dark
        upward = not upward
        col -= 2

    best_grid = unmasked
    best_score = 10**9
    for mask in range(8):
        cand = [row[:] for row in unmasked]
        for r in range(size):
            for c in range(size):
                if not is_func[r][c] and _mask_fn(mask, r, c):
                    cand[r][c] ^= 1
        fmt_data = (EC_BITS["Q"] << 3) | mask
        rem = fmt_data << 10
        for i in range(14, 9, -1):
            if (rem >> i) & 1:
                rem ^= 0x537 << (i - 10)
        fmt_bits = ((fmt_data << 10) | rem) ^ 0x5412

        def fbit(idx: int) -> int:
            return (fmt_bits >> idx) & 1

        for i in range(6):
            cand[8][i] = fbit(14 - i)
        cand[8][7] = fbit(8)
        cand[8][8] = fbit(7)
        cand[7][8] = fbit(6)
        for i in range(6):
            cand[5 - i][8] = fbit(5 - i)
        for i in range(7):
            cand[size - 1 - i][8] = fbit(14 - i)
        for i in range(8):
            cand[8][size - 8 + i] = fbit(7 - i)

        sc = _penalty(cand, size)
        if sc < best_score:
            best_score = sc
            best_grid = cand
    return best_grid


SWIFT_STAMPER = r"""
import Foundation
import PDFKit
import CoreGraphics
import CoreText
import AppKit

struct PageQR: Decodable {
    let pageNo: Int
    let code: String
    let modules: [[Int]]
}
struct StampSpec: Decodable {
    let inputPath: String
    let outputPath: String
    let qrSizeMm: Double
    let quietModules: Int
    let pages: [PageQR]
}

let specPath = CommandLine.arguments[1]
let specData = try Data(contentsOf: URL(fileURLWithPath: specPath))
let spec = try JSONDecoder().decode(StampSpec.self, from: specData)

guard let pdfDoc = PDFDocument(url: URL(fileURLWithPath: spec.inputPath)) else {
    fputs("Failed to open input PDF: \(spec.inputPath)\n", stderr)
    exit(1)
}

let outUrl = URL(fileURLWithPath: spec.outputPath) as CFURL
var mediaBox = CGRect(x: 0, y: 0, width: 595.2756, height: 841.8898) // A4 in pt
guard let ctx = CGContext(outUrl, mediaBox: &mediaBox, nil) else {
    fputs("Failed to create output PDF context\n", stderr)
    exit(1)
}

let mmToPt: CGFloat = 72.0 / 25.4
let totalPages = pdfDoc.pageCount

for i in 0..<totalPages {
    guard let page = pdfDoc.page(at: i) else { continue }
    let bounds = page.bounds(for: .mediaBox)
    var pageRect = CGRect(x: 0, y: 0, width: bounds.width, height: bounds.height)
    ctx.beginPDFPage([kCGPDFContextMediaBox as String: NSValue(rect: pageRect)] as CFDictionary)

    // Draw original vector PDF page untouched
    ctx.saveGState()
    page.draw(with: .mediaBox, to: ctx)
    ctx.restoreGState()

    let wPt = pageRect.width
    let hPt = pageRect.height
    let insetPt = 6.0 * mmToPt
    let anchorPt = 4.0 * mmToPt

    // White mask over footer strip (leaves 6..10mm corner anchors untouched)
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(CGRect(x: 11.0 * mmToPt, y: 3.8 * mmToPt, width: wPt - 22.5 * mmToPt, height: 17.2 * mmToPt))

    // 4 solid black corner registration anchors (4mm x 4mm, 6mm inset)
    ctx.setFillColor(CGColor(red: 0, green: 0, blue: 0, alpha: 1))
    ctx.fill(CGRect(x: insetPt, y: hPt - insetPt - anchorPt, width: anchorPt, height: anchorPt))
    ctx.fill(CGRect(x: wPt - insetPt - anchorPt, y: hPt - insetPt - anchorPt, width: anchorPt, height: anchorPt))
    ctx.fill(CGRect(x: insetPt, y: insetPt, width: anchorPt, height: anchorPt))
    ctx.fill(CGRect(x: wPt - insetPt - anchorPt, y: insetPt, width: anchorPt, height: anchorPt))

    // Footer vector QR (15mm x 15mm at right: 15mm -> x=180mm, bottom: 5.0mm -> y_top=277mm)
    let qrItem = spec.pages[i]
    let qrPt = CGFloat(spec.qrSizeMm) * mmToPt
    let qrX = wPt - 15.0 * mmToPt - qrPt
    let qrY = 5.0 * mmToPt
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(CGRect(x: qrX - 1.5 * mmToPt, y: qrY - 1.2 * mmToPt, width: qrPt + 3.0 * mmToPt, height: qrPt + 2.4 * mmToPt))

    let gridN = qrItem.modules.count
    let totalMods = CGFloat(gridN + spec.quietModules * 2)
    let modPt = qrPt / totalMods
    ctx.setFillColor(CGColor(red: 0, green: 0, blue: 0, alpha: 1))
    for r in 0..<gridN {
        var c = 0
        while c < gridN {
            if qrItem.modules[r][c] == 0 {
                c += 1
                continue
            }
            var run = 1
            while c + run < gridN && qrItem.modules[r][c + run] != 0 {
                run += 1
            }
            let mx = qrX + CGFloat(c + spec.quietModules) * modPt
            let my = qrY + CGFloat(gridN - 1 - r + spec.quietModules) * modPt
            ctx.fill(CGRect(x: mx, y: my, width: CGFloat(run) * modPt, height: modPt))
            c += run
        }
    }

    // Single-line code label right beside QR
    let monoFont = NSFont.monospacedSystemFont(ofSize: 7.0, weight: .regular)
    let codeAttrs: [NSAttributedString.Key: Any] = [
        .font: monoFont,
        .foregroundColor: NSColor(calibratedWhite: 0.08, alpha: 1.0)
    ]
    let codeStr = NSAttributedString(string: qrItem.code, attributes: codeAttrs)
    let codeLine = CTLineCreateWithAttributedString(codeStr)
    let codeBounds = CTLineGetBoundsWithOptions(codeLine, [])
    ctx.textPosition = CGPoint(x: qrX - 3.0 * mmToPt - codeBounds.width, y: qrY + 1.2 * mmToPt)
    CTLineDraw(codeLine, ctx)

    // Page number ("第 X 页 / 共 Y 页") centered in footer
    let songFont = NSFont(name: "Songti SC", size: 8.5) ?? NSFont.systemFont(ofSize: 8.5)
    let pgAttrs: [NSAttributedString.Key: Any] = [
        .font: songFont,
        .foregroundColor: NSColor(calibratedWhite: 0.2, alpha: 1.0)
    ]
    let pgStr = NSAttributedString(string: "第 \(qrItem.pageNo) 页 / 共 \(totalPages) 页", attributes: pgAttrs)
    let pgLine = CTLineCreateWithAttributedString(pgStr)
    let pgBounds = CTLineGetBoundsWithOptions(pgLine, [])
    ctx.textPosition = CGPoint(x: (wPt - pgBounds.width) * 0.5, y: qrY + 4.5 * mmToPt)
    CTLineDraw(pgLine, ctx)

    ctx.endPDFPage()
}
ctx.closePDF()

let matches = pdfDoc.findString("BFGRID0", withOptions: .literal)
if let firstSel = matches.first, let selPage = firstSel.pages.first {
    let pIdx = pdfDoc.index(for: selPage)
    let pb = selPage.bounds(for: .mediaBox)
    let sb = firstSel.bounds(for: selPage)
    let oxMm = max(0.0, Double(sb.origin.x / pb.width) * 210.0 - 0.35)
    let bottomSelFromTopMm = Double((pb.height - sb.origin.y) / pb.height) * 297.0
    let gridOriginYMm = max(0.0, bottomSelFromTopMm - 5.17)
    print(String(format: "BFGRID0 page=%d gridX=%.3f gridY=%.3f", pIdx + 1, oxMm, gridOriginYMm))
}
print("Stamped \(totalPages) pages -> \(spec.outputPath)")
"""


def _update_pack_with_probe(pack_path: Path, page_count: int, probe_page: int | None, gx_mm: float, gy_mm: float) -> None:
    pack = json.loads(pack_path.read_text(encoding="utf-8"))
    ab = pack.get("answer_box")
    has_cells = isinstance(ab, dict) and bool(ab.get("cells"))
    if has_cells and probe_page is None:
        raise RuntimeError(
            "【阻断封版】未在 PDF 中检测到客观题答题框定位锚点（BFGRID0），答题框可能已被误删或破坏，已拒绝更新批改包！"
        )

    if "paper" in pack and isinstance(pack["paper"], dict):
        pack["paper"]["page_count"] = page_count
    pack["pageCount"] = page_count
    pack["totalPages"] = page_count
    if not has_cells or probe_page is None:
        pack_path.write_text(json.dumps(pack, ensure_ascii=False, indent=2), encoding="utf-8")
        return

    cols = int(ab.get("cols") or 10)
    cell_w = float(ab.get("cell_w_mm") or 18.0)
    title_h = float(ab.get("title_h_mm") or 5.0)
    hdr_h = float(ab.get("header_h_mm") or 5.0)
    cell_h = float(ab.get("cell_h_mm") or 10.0)
    rows = int(ab.get("rows") or 1)
    write0_y = gy_mm + title_h + hdr_h
    pitch_y = cell_h + hdr_h
    total_w = cols * cell_w
    total_h = rows * cell_h + max(0, rows - 1) * hdr_h

    def r4(v: float) -> float:
        return round(max(0.0, min(1.0, v)), 4)

    def r2(v: float) -> float:
        return round(v, 2)

    def norm_rect(x: float, y: float, w: float, h: float) -> dict[str, float]:
        return {
            "nx": r4((x - 8.0) / 194.0),
            "ny": r4((y - 8.0) / 281.0),
            "nw": r4(w / 194.0),
            "nh": r4(h / 281.0),
        }

    for idx, cell in enumerate(ab["cells"]):
        r = int(cell.get("row", idx // cols))
        c = int(cell.get("col", idx % cols))
        cx = gx_mm + c * cell_w
        cy = write0_y + r * pitch_y
        rm = {"x": r2(cx), "y": r2(cy), "w": r2(cell_w), "h": r2(cell_h)}
        cell["row"] = r
        cell["col"] = c
        cell["rectMm"] = rm
        cell["bbox_mm"] = rm
        cell["normRect"] = norm_rect(cx, cy, cell_w, cell_h)
        cell["x"] = r4(cx / 210.0)
        cell["y"] = r4(cy / 297.0)
        cell["w"] = r4(cell_w / 210.0)
        cell["h"] = r4(cell_h / 297.0)

    grid_origin = {"x": r2(gx_mm), "y": r2(gy_mm)}
    box_mm = {"x": r2(gx_mm), "y": r2(write0_y), "w": r2(total_w), "h": r2(total_h)}
    box_norm = norm_rect(gx_mm, write0_y, total_w, total_h)
    ab["anchorToken"] = "BFGRID0"
    ab["page"] = probe_page
    ab["gridOriginMm"] = grid_origin
    ab["rectMm"] = box_mm
    ab["bbox_mm"] = box_mm
    ab["normRect"] = box_norm
    ab["x"] = r4(gx_mm / 210.0)
    ab["y"] = r4(write0_y / 297.0)
    ab["w"] = r4(total_w / 210.0)
    ab["h"] = r4(total_h / 297.0)
    ab["bbox"] = {"x": ab["x"], "y": ab["y"], "w": ab["w"], "h": ab["h"]}
    if isinstance(pack.get("objectiveAnswerGrid"), dict):
        og = pack["objectiveAnswerGrid"]
        og["anchorToken"] = "BFGRID0"
        og["page"] = probe_page
        og["gridOriginMm"] = grid_origin
        og["rectMm"] = box_mm
        og["normRect"] = box_norm
        og["cells"] = ab["cells"]
    pack_path.write_text(json.dumps(pack, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"Updated answer_box in {pack_path} -> page {probe_page}, gridOriginMm=({gx_mm:.2f}mm, {gy_mm:.2f}mm)")


def stamp_pdf(
    input_pdf: Path,
    output_pdf: Path,
    paper_id: str,
    qr_size_mm: float = 15.0,
    quiet: int = 0,
    update_pack: Path | None = None,
) -> int:
    if not input_pdf.is_file():
        raise FileNotFoundError(f"Input PDF not found: {input_pdf}")
    pages_spec = []
    for p in range(1, 65):
        code = f"BF:{paper_id}:{p}"
        pages_spec.append({"pageNo": p, "code": code, "modules": qr_matrix_q(code)})

    output_pdf.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        tmp_dir = Path(tmp)
        swift_file = tmp_dir / "stamp.swift"
        spec_file = tmp_dir / "spec.json"
        swift_file.write_text(SWIFT_STAMPER, encoding="utf-8")
        spec_file.write_text(
            json.dumps(
                {
                    "inputPath": str(input_pdf.resolve()),
                    "outputPath": str(output_pdf.resolve()),
                    "qrSizeMm": qr_size_mm,
                    "quietModules": quiet,
                    "pages": pages_spec,
                }
            ),
            encoding="utf-8",
        )
        res = subprocess.run(["swift", str(swift_file), str(spec_file)], capture_output=True, text=True)
        if res.returncode != 0:
            raise RuntimeError(f"PDF stamp failed:\n{res.stderr}")
        out_lines = [ln.strip() for ln in res.stdout.splitlines() if ln.strip()]
        probe_hit = None
        stamped_pages = 0
        for ln in out_lines:
            print(ln)
            if ln.startswith("BFGRID0 "):
                kv = dict(tok.split("=") for tok in ln.split()[1:])
                probe_hit = (int(kv["page"]), float(kv["gridX"]), float(kv["gridY"]))
            elif ln.startswith("Stamped "):
                parts = ln.split()
                if len(parts) >= 2 and parts[1].isdigit():
                    stamped_pages = int(parts[1])
        if update_pack and update_pack.is_file():
            if probe_hit:
                _update_pack_with_probe(update_pack, stamped_pages, probe_hit[0], probe_hit[1], probe_hit[2])
            else:
                # Will raise RuntimeError if objective cells exist in pack!
                output_pdf.unlink(missing_ok=True)
                _update_pack_with_probe(update_pack, stamped_pages, None, 0.0, 0.0)
        return stamped_pages


def main() -> int:
    ap = argparse.ArgumentParser(description="Stamp Biofolio 15mm vector footer QR + page numbers onto an A4 PDF (scan at 300 DPI).")
    ap.add_argument("input_pdf", type=Path, help="Input single-section A4 PDF (e.g. exported from Word)")
    ap.add_argument("output_pdf", type=Path, help="Output stamped A4 PDF")
    ap.add_argument("--paper-id", required=True, help="10-char paper_id (matches grading pack JSON)")
    ap.add_argument("--qr-mm", type=float, default=15.0, help="Footer QR size in mm (default: 15.0 -> 13.1mm on 260x368 8K)")
    ap.add_argument("--update-pack", type=Path, default=None, help="Optional grading pack JSON to update via BFGRID0 anchor")
    args = ap.parse_args()
    stamp_pdf(args.input_pdf, args.output_pdf, args.paper_id.strip(), qr_size_mm=args.qr_mm, update_pack=args.update_pack)
    return 0


if __name__ == "__main__":
    sys.exit(main())
