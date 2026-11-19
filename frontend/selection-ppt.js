// Client-side PPTX (OOXML) lecture deck exporter, A4 print/PDF renderer, and classroom presenter.
const PPT_W_IN = 13.333333;
const PPT_H_IN = 7.5;
const PPT_EMU = 914400;
const PPT_PT = 12700;
const pptIn = v => Math.round(v * PPT_EMU);
const pptPt = v => Math.round(v * PPT_PT);

function pptXmlEscape(text) {
  return String(text ?? '')
    .replace(/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/g, '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;');
}

// Parse PNG / JPEG / GIF dimensions directly from raw bytes so both browser and Node work.
function pptImageDimensions(bytes, mime) {
  if (!bytes || bytes.length < 24) return { width: 640, height: 480 };
  if (/png/i.test(mime) && bytes[0] === 0x89 && bytes[1] === 0x50) {
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const width = view.getUint32(16, false);
    const height = view.getUint32(20, false);
    if (width > 0 && height > 0) return { width, height };
  }
  if (/gif/i.test(mime) && bytes[0] === 0x47 && bytes[1] === 0x49) {
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const width = view.getUint16(6, true);
    const height = view.getUint16(8, true);
    if (width > 0 && height > 0) return { width, height };
  }
  if (/jpe?g/i.test(mime) && bytes[0] === 0xff && bytes[1] === 0xd8) {
    let i = 2;
    while (i + 9 < bytes.length) {
      if (bytes[i] !== 0xff) { i++; continue; }
      const marker = bytes[i + 1];
      if (marker === 0xd8 || marker === 0xd9 || (marker >= 0xd0 && marker <= 0xd7)) { i += 2; continue; }
      const len = (bytes[i + 2] << 8) | bytes[i + 3];
      if (len < 2 || i + 2 + len > bytes.length) break;
      if ((marker >= 0xc0 && marker <= 0xc3) || (marker >= 0xc5 && marker <= 0xc7) || (marker >= 0xc9 && marker <= 0xcb) || (marker >= 0xcd && marker <= 0xcf)) {
        const height = (bytes[i + 5] << 8) | bytes[i + 6];
        const width = (bytes[i + 7] << 8) | bytes[i + 8];
        if (width > 0 && height > 0) return { width, height };
      }
      i += 2 + len;
    }
  }
  return { width: 640, height: 480 };
}

async function pptLoadImageAsset(src) {
  let url = await inlinePicture(src);
  if (!url || !url.startsWith('data:image/')) throw Error('配图无法读取');
  const head = url.slice(0, url.indexOf(','));
  let mime = (head.match(/^data:(image\/[a-z0-9+.-]+);base64$/i)?.[1] || 'image/png').toLowerCase();
  if (mime === 'image/webp' || !/^image\/(png|jpe?g|gif)$/.test(mime)) {
    if (typeof document !== 'undefined' && typeof Image !== 'undefined') {
      const img = new Image();
      await new Promise((resolve, reject) => {
        img.onload = resolve;
        img.onerror = () => reject(Error('配图无法转换'));
        img.src = url;
        if (img.complete && img.naturalWidth) resolve();
      });
      const canvas = document.createElement('canvas');
      canvas.width = img.naturalWidth;
      canvas.height = img.naturalHeight;
      canvas.getContext('2d').drawImage(img, 0, 0);
      url = canvas.toDataURL('image/png');
      mime = 'image/png';
    }
  }
  const b64 = url.slice(url.indexOf(',') + 1);
  let bytes;
  if (typeof Uint8Array !== 'undefined' && typeof atob === 'function') {
    const bin = atob(b64);
    bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  } else {
    bytes = new Uint8Array(Buffer.from(b64, 'base64'));
  }
  let width = 0, height = 0;
  if (typeof Image !== 'undefined' && typeof document !== 'undefined') {
    try {
      const img = new Image();
      await new Promise((resolve, reject) => {
        img.onload = resolve;
        img.onerror = reject;
        img.src = url;
        if (img.complete && img.naturalWidth) resolve();
      });
      width = img.naturalWidth;
      height = img.naturalHeight;
    } catch {}
  }
  if (!width || !height) ({ width, height } = pptImageDimensions(bytes, mime));
  const ext = mime === 'image/jpeg' || mime === 'image/jpg' ? 'jpg' : mime === 'image/gif' ? 'gif' : 'png';
  return { ext, mime: ext === 'jpg' ? 'image/jpeg' : 'image/' + ext, bytes, width, height };
}

function htmlToPptBlocks(html) {
  if (!html) return [];
  if (typeof document === 'undefined') {
    return String(html).replace(/<[^>]+>/g, '\n').split(/\n+/).map(t => t.trim()).filter(Boolean).map(text => ({
      type: 'p',
      runs: [{ text, bold: false, italic: false, underline: false, sup: false, sub: false }]
    }));
  }
  const tpl = document.createElement('template');
  tpl.innerHTML = safeHtml(html);
  const blocks = [];
  let runs = [];
  function flushPara() {
    const cleaned = [];
    for (const r of runs) {
      if (!r.text) continue;
      const prev = cleaned[cleaned.length - 1];
      if (prev && prev.bold === r.bold && prev.italic === r.italic && prev.underline === r.underline && prev.sup === r.sup && prev.sub === r.sub && !r.br && !prev.br) {
        prev.text += r.text;
      } else {
        cleaned.push({ ...r });
      }
    }
    if (cleaned.length) blocks.push({ type: 'p', runs: cleaned });
    runs = [];
  }
  function extractRuns(node, style = {}) {
    if (node.nodeType === 3) {
      const text = node.textContent;
      if (text) runs.push({ text, bold: !!style.bold, italic: !!style.italic, underline: !!style.underline, sup: !!style.sup, sub: !!style.sub });
      return;
    }
    if (node.nodeType !== 1) return;
    const tag = node.nodeName;
    if (tag === 'BR') {
      runs.push({ br: true, text: '\n' });
      return;
    }
    if (tag === 'IMG') {
      flushPara();
      const src = node.getAttribute('src');
      if (src) blocks.push({ type: 'img', src });
      return;
    }
    if (tag === 'TABLE') {
      flushPara();
      const rows = [];
      for (const tr of node.rows || []) {
        const row = [];
        for (const cell of tr.cells || []) {
          const cellRuns = [];
          const savedRuns = runs;
          runs = [];
          for (const child of cell.childNodes) {
            if (child.nodeName === 'P' || child.nodeName === 'DIV') {
              if (runs.length) runs.push({ br: true, text: '\n' });
              for (const gc of child.childNodes) extractRuns(gc, style);
            } else {
              extractRuns(child, style);
            }
          }
          for (const r of runs) cellRuns.push(r);
          runs = savedRuns;
          row.push({
            runs: cellRuns.length ? cellRuns : [{ text: '', bold: false, italic: false, underline: false, sup: false, sub: false }],
            colspan: Math.max(1, Number(cell.getAttribute('colspan')) || 1),
            rowspan: Math.max(1, Number(cell.getAttribute('rowspan')) || 1)
          });
        }
        if (row.length) rows.push(row);
      }
      if (rows.length) blocks.push({ type: 'table', rows });
      return;
    }
    const nextStyle = { ...style };
    if (tag === 'B' || tag === 'STRONG') nextStyle.bold = true;
    if (tag === 'I' || tag === 'EM') nextStyle.italic = true;
    if (tag === 'U') nextStyle.underline = true;
    if (tag === 'SUP') nextStyle.sup = true;
    if (tag === 'SUB') nextStyle.sub = true;
    const isBlock = /^(P|DIV|LI|H[1-6]|BLOCKQUOTE)$/.test(tag);
    if (isBlock) flushPara();
    for (const child of node.childNodes) extractRuns(child, nextStyle);
    if (isBlock) flushPara();
  }
  for (const child of tpl.content.childNodes) extractRuns(child, {});
  flushPara();
  return blocks;
}

function pptCharUnits(text) {
  let units = 0;
  for (const ch of String(text || '')) {
    units += ch.charCodeAt(0) > 127 ? 1.02 : 0.56;
  }
  return units;
}

function estimatePptLines(runs, widthIn, fontSizePt) {
  const usablePt = Math.max(60, widthIn * 72 - 14);
  const unitsPerLine = Math.max(6, usablePt / fontSizePt);
  let lines = 1, cur = 0;
  for (const r of runs) {
    if (r.br) { lines++; cur = 0; continue; }
    const parts = String(r.text || '').split('\n');
    for (let i = 0; i < parts.length; i++) {
      if (i > 0) { lines++; cur = 0; }
      const u = pptCharUnits(parts[i]);
      if (cur + u <= unitsPerLine) {
        cur += u;
      } else {
        const left = Math.max(0, u - (unitsPerLine - cur));
        const extra = Math.ceil(left / unitsPerLine);
        lines += extra;
        cur = left - (extra - 1) * unitsPerLine;
      }
    }
  }
  return Math.max(1, lines);
}

function normalizePptTableGrid(rows) {
  const grid = [];
  let maxCols = 0;
  for (let rIdx = 0; rIdx < rows.length; rIdx++) {
    if (!grid[rIdx]) grid[rIdx] = [];
    let cIdx = 0;
    for (const cell of rows[rIdx]) {
      while (grid[rIdx][cIdx]) cIdx++;
      for (let dr = 0; dr < cell.rowspan; dr++) {
        if (!grid[rIdx + dr]) grid[rIdx + dr] = [];
        for (let dc = 0; dc < cell.colspan; dc++) {
          if (dr === 0 && dc === 0) {
            grid[rIdx][cIdx] = { anchor: true, runs: cell.runs, colspan: cell.colspan, rowspan: cell.rowspan };
          } else {
            grid[rIdx + dr][cIdx + dc] = { anchor: false, hMerge: dc > 0, vMerge: dr > 0 };
          }
        }
      }
      cIdx += cell.colspan;
      if (cIdx > maxCols) maxCols = cIdx;
    }
  }
  for (const row of grid) {
    for (let c = 0; c < maxCols; c++) {
      if (!row[c]) row[c] = { anchor: true, runs: [{ text: '' }], colspan: 1, rowspan: 1 };
    }
  }
  return { grid, numCols: Math.max(1, maxCols) };
}

function estimatePptBlockPt(block, widthIn, fontSizePt) {
  if (block.type === 'p') {
    const lines = estimatePptLines(block.runs, widthIn, fontSizePt);
    return lines * fontSizePt * 1.42 + fontSizePt * 0.38;
  }
  if (block.type === 'table') {
    const { grid, numCols } = normalizePptTableGrid(block.rows);
    const colWidthIn = Math.max(0.7, widthIn / numCols);
    const tblFont = Math.min(fontSizePt, 14);
    let total = 8;
    for (const row of grid) {
      let maxLines = 1;
      for (const cell of row) {
        if (cell.anchor) maxLines = Math.max(maxLines, estimatePptLines(cell.runs, colWidthIn * cell.colspan, tblFont));
      }
      total += maxLines * tblFont * 1.35 + 10;
    }
    return total + 10;
  }
  return 0;
}

function splitParagraphIfHuge(block, widthIn, fontSizePt, maxPt) {
  if (block.type !== 'p' || estimatePptBlockPt(block, widthIn, fontSizePt) <= maxPt) return [block];
  const full = block.runs.map(r => r.text).join('');
  if (full.length < 40) return [block];
  // Split at sentence boundaries while preserving plain styling for ultra-long stems.
  const sentences = full.split(/(?<=[。；！？\n])/).filter(Boolean);
  if (sentences.length <= 1) return [block];
  const out = [];
  let buf = '';
  for (const s of sentences) {
    const candidate = buf + s;
    const est = estimatePptBlockPt({ type: 'p', runs: [{ text: candidate }] }, widthIn, fontSizePt);
    if (buf && est > maxPt * 0.85) {
      out.push({ type: 'p', runs: [{ text: buf.trimEnd() }] });
      buf = s;
    } else {
      buf = candidate;
    }
  }
  if (buf.trim()) out.push({ type: 'p', runs: [{ text: buf.trimEnd() }] });
  return out.length ? out : [block];
}

function layoutTextPages(textBlocks, widthIn, maxHeightPt) {
  const candidates = [21, 18, 16, 14.5, 13.5];
  let chosenFont = 13.5;
  for (const sz of candidates) {
    const sum = textBlocks.reduce((acc, b) => acc + estimatePptBlockPt(b, widthIn, sz), 0);
    if (sum <= maxHeightPt * 0.92) { chosenFont = sz; break; }
  }
  const expanded = textBlocks.flatMap(b => splitParagraphIfHuge(b, widthIn, chosenFont, maxHeightPt * 0.9));
  const pages = [];
  let current = [], used = 0;
  for (const b of expanded) {
    const h = estimatePptBlockPt(b, widthIn, chosenFont);
    if (current.length && used + h > maxHeightPt * 0.94) {
      pages.push(current);
      current = [b];
      used = h;
    } else {
      current.push(b);
      used += h;
    }
  }
  if (current.length) pages.push(current);
  return { fontSizePt: chosenFont, pages: pages.length ? pages : [[]] };
}

function renderPptRunsXml(runs, fontSizePt, defaultColor = '1F2A24', extraBold = false) {
  const sz = Math.round(fontSizePt * 100);
  const out = [];
  for (const r of runs) {
    if (r.br) {
      out.push(`<a:br><a:rPr lang="zh-CN" altLang="en-US" sz="${sz}"/></a:br>`);
      continue;
    }
    const text = String(r.text || '');
    if (!text) continue;
    const attrs = [`lang="zh-CN"`, `altLang="en-US"`, `sz="${sz}"`];
    if (r.bold || extraBold) attrs.push('b="1"');
    if (r.italic) attrs.push('i="1"');
    if (r.underline) attrs.push('u="sng"');
    if (r.sup) attrs.push('baseline="30000"');
    else if (r.sub) attrs.push('baseline="-25000"');
    const color = r.color || defaultColor;
    out.push(`<a:r><a:rPr ${attrs.join(' ')}><a:solidFill><a:srgbClr val="${color}"/></a:solidFill><a:latin typeface="Calibri"/><a:ea typeface="微软雅黑"/><a:cs typeface="Calibri"/></a:rPr><a:t>${pptXmlEscape(text)}</a:t></a:r>`);
  }
  if (!out.length) {
    out.push(`<a:r><a:rPr lang="zh-CN" sz="${sz}"/><a:t></a:t></a:r>`);
  }
  return out.join('');
}

function renderPptParagraphsXml(blocks, fontSizePt, defaultColor = '1F2A24') {
  const sz = Math.round(fontSizePt * 100);
  const paras = [];
  for (const b of blocks) {
    if (b.type !== 'p') continue;
    paras.push(`<a:p><a:pPr algn="l" spcAft="450"><a:lnSpc><a:spcPct val="125000"/></a:lnSpc></a:pPr>${renderPptRunsXml(b.runs, fontSizePt, defaultColor)}<a:endParaRPr lang="zh-CN" sz="${sz}"/></a:p>`);
  }
  if (!paras.length) paras.push(`<a:p><a:endParaRPr lang="zh-CN" sz="${sz}"/></a:p>`);
  return paras.join('');
}

function renderPptTableShape(id, xIn, yIn, wIn, hIn, rows, fontSizePt) {
  const { grid, numCols } = normalizePptTableGrid(rows);
  const colW = Math.round(pptIn(wIn) / numCols);
  const tblFont = Math.min(fontSizePt, 14);
  const sz = Math.round(tblFont * 100);
  const gridCols = Array.from({ length: numCols }, () => `<a:gridCol w="${colW}"/>`).join('');
  const rowH = Math.max(pptPt(24), Math.round(pptIn(hIn) / Math.max(1, grid.length)));
  const trXml = grid.map((row, rIdx) => {
    const cells = row.map(cell => {
      const attrs = [];
      if (cell.anchor) {
        if (cell.colspan > 1) attrs.push(`gridSpan="${cell.colspan}"`);
        if (cell.rowspan > 1) attrs.push(`rowSpan="${cell.rowspan}"`);
      } else {
        if (cell.hMerge) attrs.push('hMerge="1"');
        if (cell.vMerge) attrs.push('vMerge="1"');
      }
      const pXml = cell.anchor
        ? `<a:p><a:pPr algn="ctr"/><a:r><a:rPr lang="zh-CN" altLang="en-US" sz="${sz}"${rIdx === 0 ? ' b="1"' : ''}><a:solidFill><a:srgbClr val="1F2A24"/></a:solidFill><a:latin typeface="Calibri"/><a:ea typeface="微软雅黑"/></a:rPr><a:t>${pptXmlEscape(cell.runs.map(r => r.text).join(''))}</a:t></a:r><a:endParaRPr lang="zh-CN" sz="${sz}"/></a:p>`
        : `<a:p><a:endParaRPr lang="zh-CN" sz="${sz}"/></a:p>`;
      const fill = rIdx === 0 ? '<a:solidFill><a:srgbClr val="E8EEE3"/></a:solidFill>' : '<a:solidFill><a:srgbClr val="FFFFFF"/></a:solidFill>';
      const ln = w => `<a:ln${w}><a:solidFill><a:srgbClr val="C8D2CA"/></a:solidFill></a:ln${w}>`;
      return `<a:tc${attrs.length ? ' ' + attrs.join(' ') : ''}><a:txBody><a:bodyPr wrap="square" anchor="ctr"/><a:lstStyle/>${pXml}</a:txBody><a:tcPr marL="60000" marR="60000" marT="40000" marB="40000" anchor="ctr">${ln('L')}${ln('R')}${ln('T')}${ln('B')}${fill}</a:tcPr></a:tc>`;
    }).join('');
    return `<a:tr h="${rowH}">${cells}</a:tr>`;
  }).join('');
  return `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="${id}" name="Table ${id}"/><p:cNvGraphicFramePr><a:graphicFrameLocks noGrp="1"/></p:cNvGraphicFramePr><p:nvPr/></p:nvGraphicFramePr><p:xfrm><a:off x="${pptIn(xIn)}" y="${pptIn(yIn)}"/><a:ext cx="${pptIn(wIn)}" cy="${pptIn(hIn)}"/></p:xfrm><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr firstRow="1" bandRow="1"/><a:tblGrid>${gridCols}</a:tblGrid>${trXml}</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`;
}

function renderPptTextBox(id, xIn, yIn, wIn, hIn, parasXml, opts = {}) {
  const fill = opts.fill ? `<a:solidFill><a:srgbClr val="${opts.fill}"/></a:solidFill>` : '<a:noFill/>';
  const line = opts.border ? `<a:ln w="${opts.borderW || 12700}"><a:solidFill><a:srgbClr val="${opts.border}"/></a:solidFill></a:ln>` : '<a:ln><a:noFill/></a:ln>';
  const shape = opts.round ? 'roundRect' : 'rect';
  const av = opts.round ? '<a:avLst><a:gd name="adj" fmla="val 25000"/></a:avLst>' : '<a:avLst/>';
  const anchor = opts.anchor || 't';
  const mar = opts.pad !== undefined ? opts.pad : 72000;
  return `<p:sp><p:nvSpPr><p:cNvPr id="${id}" name="Box ${id}"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="${pptIn(xIn)}" y="${pptIn(yIn)}"/><a:ext cx="${pptIn(wIn)}" cy="${pptIn(hIn)}"/></a:xfrm><a:prstGeom prst="${shape}">${av}</a:prstGeom>${fill}${line}</p:spPr><p:txBody><a:bodyPr wrap="square" rtlCol="0" anchor="${anchor}" lIns="${mar}" rIns="${mar}" tIns="${Math.round(mar * 0.7)}" bIns="${Math.round(mar * 0.7)}"/><a:lstStyle/>${parasXml}</p:txBody></p:sp>`;
}

function renderPptRect(id, xIn, yIn, wIn, hIn, fillHex) {
  return `<p:sp><p:nvSpPr><p:cNvPr id="${id}" name="Rect ${id}"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="${pptIn(xIn)}" y="${pptIn(yIn)}"/><a:ext cx="${pptIn(wIn)}" cy="${pptIn(hIn)}"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom><a:solidFill><a:srgbClr val="${fillHex}"/></a:solidFill><a:ln><a:noFill/></a:ln></p:spPr></p:sp>`;
}

function renderPptPicture(id, rId, xIn, yIn, wIn, hIn) {
  return `<p:pic><p:nvPicPr><p:cNvPr id="${id}" name="Picture ${id}"/><p:cNvPicPr><a:picLocks noChangeAspect="1"/></p:cNvPicPr><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="${rId}"/><a:stretch><a:fillRect/></a:stretch></p:blipFill><p:spPr><a:xfrm><a:off x="${pptIn(xIn)}" y="${pptIn(yIn)}"/><a:ext cx="${pptIn(wIn)}" cy="${pptIn(hIn)}"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom><a:ln w="9525"><a:solidFill><a:srgbClr val="D9DFD8"/></a:solidFill></a:ln></p:spPr></p:pic>`;
}

function buildSlideHeaderAndFooter(nextId, titleBadge, subTag, statBadge, footerLeft, pageLabel) {
  const shapes = [];
  shapes.push(renderPptRect(nextId(), 0, 0, PPT_W_IN, 0.07, '2F6B4F'));
  // Left badge pill
  const badgeW = Math.min(4.8, Math.max(1.8, pptCharUnits(titleBadge) * 0.23 + 0.55));
  shapes.push(renderPptTextBox(nextId(), 0.55, 0.24, badgeW, 0.44,
    `<a:p><a:pPr algn="ctr"/>${renderPptRunsXml([{ text: titleBadge, bold: true, color: 'FFFFFF' }], 15, 'FFFFFF', true)}<a:endParaRPr lang="zh-CN" sz="1500"/></a:p>`,
    { fill: '2F6B4F', round: true, anchor: 'ctr', pad: 40000 }));
  if (subTag) {
    shapes.push(renderPptTextBox(nextId(), 0.55 + badgeW + 0.14, 0.25, 4.8, 0.42,
      `<a:p><a:pPr algn="l"/>${renderPptRunsXml([{ text: subTag, color: '67716B' }], 13, '67716B')}<a:endParaRPr lang="zh-CN" sz="1300"/></a:p>`,
      { anchor: 'ctr', pad: 20000 }));
  }
  if (statBadge) {
    const sw = Math.min(4.4, Math.max(2.0, pptCharUnits(statBadge.text) * 0.17 + 0.45));
    shapes.push(renderPptTextBox(nextId(), PPT_W_IN - 0.55 - sw, 0.24, sw, 0.42,
      `<a:p><a:pPr algn="ctr"/>${renderPptRunsXml([{ text: statBadge.text, bold: true, color: statBadge.hot ? 'A5611A' : '4A5A50' }], 12.5, statBadge.hot ? 'A5611A' : '4A5A50', true)}<a:endParaRPr lang="zh-CN" sz="1250"/></a:p>`,
      { fill: statBadge.hot ? 'FFF2DF' : 'E8EEE3', border: statBadge.hot ? 'E5B97A' : 'C7D6CB', round: true, anchor: 'ctr', pad: 30000 }));
  }
  // Thin divider under header & footer
  shapes.push(renderPptRect(nextId(), 0.55, 0.77, PPT_W_IN - 1.1, 0.015, 'DCE2DC'));
  shapes.push(renderPptRect(nextId(), 0.55, 7.03, PPT_W_IN - 1.1, 0.012, 'DCE2DC'));
  shapes.push(renderPptTextBox(nextId(), 0.55, 7.06, 9.8, 0.34,
    `<a:p><a:pPr algn="l"/>${renderPptRunsXml([{ text: footerLeft, color: '67716B' }], 11, '67716B')}<a:endParaRPr lang="zh-CN" sz="1100"/></a:p>`,
    { anchor: 'ctr', pad: 15000 }));
  shapes.push(renderPptTextBox(nextId(), PPT_W_IN - 2.3, 7.06, 1.75, 0.34,
    `<a:p><a:pPr algn="r"/>${renderPptRunsXml([{ text: pageLabel, color: '67716B' }], 11, '67716B')}<a:endParaRPr lang="zh-CN" sz="1100"/></a:p>`,
    { anchor: 'ctr', pad: 15000 }));
  return shapes;
}

function fitImagesInBox(imgs, boxX, boxY, boxW, boxH) {
  if (!imgs.length) return [];
  const gap = 0.14;
  const maxEachH = (boxH - gap * (imgs.length - 1)) / imgs.length;
  const placed = [];
  let totalH = 0;
  const sized = imgs.map(img => {
    const ratio = img.width / Math.max(1, img.height);
    let w = boxW, h = w / ratio;
    if (h > maxEachH) { h = maxEachH; w = h * ratio; }
    totalH += h;
    return { img, w, h };
  });
  totalH += gap * (sized.length - 1);
  let y = boxY + Math.max(0, (boxH - totalH) / 2);
  for (const s of sized) {
    const x = boxX + Math.max(0, (boxW - s.w) / 2);
    placed.push({ img: s.img, x, y, w: s.w, h: s.h });
    y += s.h + gap;
  }
  return placed;
}

function renderContentBlockSequence(nextId, seq, xIn, yIn, wIn, hIn, fontSizePt, cardBg = true) {
  const shapes = [];
  if (cardBg) {
    shapes.push(renderPptTextBox(nextId(), xIn, yIn, wIn, hIn, '<a:p><a:endParaRPr lang="zh-CN" sz="1200"/></a:p>',
      { fill: 'FFFFFF', border: 'DCE2DC', borderW: 9525, pad: 0 }));
  }
  const padX = 0.18, padY = 0.14;
  const innerW = wIn - padX * 2;
  let curY = yIn + padY;
  const maxBottom = yIn + hIn - padY;
  // Group adjacent paragraphs to minimize shape clutter while keeping tables distinct.
  let paraBuf = [];
  const flushBuf = () => {
    if (!paraBuf.length) return;
    const estPt = paraBuf.reduce((acc, p) => acc + estimatePptBlockPt(p, innerW, fontSizePt), 0);
    const boxH = Math.min(Math.max(0.35, estPt / 72 + 0.12), Math.max(0.35, maxBottom - curY));
    shapes.push(renderPptTextBox(nextId(), xIn + padX, curY, innerW, boxH, renderPptParagraphsXml(paraBuf, fontSizePt), { pad: 24000 }));
    curY += boxH + 0.04;
    paraBuf = [];
  };
  for (const b of seq) {
    if (b.type === 'table') {
      flushBuf();
      const estPt = estimatePptBlockPt(b, innerW, fontSizePt);
      const tblH = Math.min(Math.max(0.6, estPt / 72), Math.max(0.6, maxBottom - curY));
      shapes.push(renderPptTableShape(nextId(), xIn + padX, curY, innerW, tblH, b.rows, fontSizePt));
      curY += tblH + 0.10;
    } else if (b.type === 'p') {
      paraBuf.push(b);
    }
  }
  flushBuf();
  return shapes;
}

async function makeEditionPptx(edition, items, options = {}) {
  if (typeof loadTool === 'function') await loadTool('zip');
  if (typeof JSZip === 'undefined') throw Error('压缩组件未加载，请刷新重试。');
  const opts = {
    answerMode: options.answerMode || 'notes', // 'notes' | 'nextSlide' | 'none'
    includeCover: options.includeCover !== false,
    showStats: options.showStats !== false,
    showSource: options.showSource !== false,
    includeTeachingNotes: options.includeTeachingNotes !== false
  };
  const zip = new JSZip();
  const mediaList = [];
  const mediaMap = new Map();
  const usedExts = new Set();

  async function registerImage(src) {
    if (mediaMap.has(src)) return mediaMap.get(src);
    const asset = await pptLoadImageAsset(src);
    const index = mediaList.length + 1;
    const filename = `image${index}.${asset.ext}`;
    const entry = { index, filename, path: `ppt/media/${filename}`, ...asset };
    mediaList.push(entry);
    mediaMap.set(src, entry);
    usedExts.add(asset.ext);
    zip.file(entry.path, asset.bytes);
    return entry;
  }

  const slides = [];
  const contentTop = 0.89;
  const contentBottom = 6.91;
  const contentH = contentBottom - contentTop;
  const fullW = PPT_W_IN - 1.1;

  if (opts.includeCover) {
    const scopeNote = items.some(it => it.stat) ? `共 ${items.length} 道精选讲评题目` : `共 ${items.length} 道题目`;
    const subParts = [edition.purpose || '课堂讲评', edition.date || '', scopeNote].filter(Boolean).join('　·　');
    slides.push({
      notes: `${edition.name}\n${subParts}`,
      build(nextId) {
        return [
          renderPptRect(nextId(), 0, 0, PPT_W_IN, 0.14, '2F6B4F'),
          renderPptRect(nextId(), 0.9, 1.85, 0.12, 3.3, '2F6B4F'),
          renderPptTextBox(nextId(), 1.25, 1.9, 11.1, 0.45,
            `<a:p>${renderPptRunsXml([{ text: '生物题册 · 课堂讲评课件', bold: true, color: '2F6B4F' }], 16, '2F6B4F', true)}<a:endParaRPr lang="zh-CN" sz="1600"/></a:p>`),
          renderPptTextBox(nextId(), 1.25, 2.45, 11.1, 1.55,
            `<a:p>${renderPptRunsXml([{ text: edition.name || '生物试卷讲评', bold: true, color: '1F2A24' }], 34, '1F2A24', true)}<a:endParaRPr lang="zh-CN" sz="3400"/></a:p>`),
          renderPptTextBox(nextId(), 1.25, 4.15, 11.1, 0.75,
            `<a:p>${renderPptRunsXml([{ text: subParts, color: '67716B' }], 18, '67716B')}<a:endParaRPr lang="zh-CN" sz="1800"/></a:p>`),
          renderPptTextBox(nextId(), 1.25, 6.2, 11.1, 0.45,
            `<a:p>${renderPptRunsXml([{ text: 'Biofolio 高中生物教师工作区', color: '8AA596' }], 13, '8AA596')}<a:endParaRPr lang="zh-CN" sz="1300"/></a:p>`)
        ];
      },
      images: []
    });
  }

  for (const item of items) {
    const faceHtml = item.faceHtml || '';
    const rawBlocks = htmlToPptBlocks(faceHtml);
    const textBlocks = rawBlocks.filter(b => b.type !== 'img');
    const imgSrcs = rawBlocks.filter(b => b.type === 'img').map(b => b.src);
    const loadedImgs = [];
    for (const src of imgSrcs) {
      try { loadedImgs.push(await registerImage(src)); } catch {}
    }

    const ansText = htmlToPptBlocks(item.answerHtml || '').map(b => b.type === 'p' ? b.runs.map(r => r.text).join('') : '[表格]').join('\n').trim();
    const anaText = htmlToPptBlocks(item.analysisHtml || '').map(b => b.type === 'p' ? b.runs.map(r => r.text).join('') : '[表格]').join('\n').trim();
    const noteLines = [];
    if (opts.answerMode !== 'none') {
      if (ansText) noteLines.push('【参考答案】\n' + ansText);
      if (anaText && opts.answerMode === 'notes') noteLines.push('【解析】\n' + anaText);
      if (item.ruleNote && opts.answerMode === 'notes') noteLines.push('【评分细则】\n' + item.ruleNote);
    }
    if (opts.includeTeachingNotes && item.teachingNote) {
      noteLines.push('【教师讲评要点】\n' + item.teachingNote.trim());
    }
    if (item.source && opts.showSource) noteLines.push('来源：' + item.source);
    const speakerNotes = noteLines.join('\n\n');

    const statBadge = (opts.showStats && item.statText) ? { text: item.statText, hot: !!item.statHot } : null;
    const footerLeft = [edition.name, opts.showSource && item.source ? '出处：' + item.source : ''].filter(Boolean).join('　|　');
    const qTitle = `第 ${item.n} 题`;
    const qSub = [item.typeLabel, item.points ? `${item.points} 分` : ''].filter(Boolean).join(' · ');

    const hasSideImgs = loadedImgs.length > 0 && loadedImgs.length <= 2 && loadedImgs.every(i => i.width / Math.max(1, i.height) <= 2.2);
    const hasBottomImgs = loadedImgs.length > 0 && !hasSideImgs;

    if (hasSideImgs) {
      const textW = 7.05;
      const imgW = 4.95;
      const imgX = 0.55 + textW + 0.23;
      const layout = layoutTextPages(textBlocks, textW, contentH * 72);
      const firstSeq = layout.pages[0];
      const placedImgs = fitImagesInBox(loadedImgs, imgX, contentTop + 0.12, imgW, contentH - 0.24);
      slides.push({
        notes: speakerNotes,
        images: placedImgs.map(p => p.img),
        build(nextId, slideImages, pageLabel) {
          const out = buildSlideHeaderAndFooter(nextId, qTitle, qSub, statBadge, footerLeft, pageLabel);
          out.push(...renderContentBlockSequence(nextId, firstSeq, 0.55, contentTop, textW, contentH, layout.fontSizePt, true));
          out.push(renderPptTextBox(nextId(), imgX - 0.08, contentTop, imgW + 0.16, contentH, '<a:p><a:endParaRPr lang="zh-CN" sz="1200"/></a:p>',
            { fill: 'FFFFFF', border: 'DCE2DC', borderW: 9525, pad: 0 }));
          for (const p of placedImgs) {
            const relId = slideImages.get(p.img);
            out.push(renderPptPicture(nextId(), relId, p.x, p.y, p.w, p.h));
          }
          return out;
        }
      });
      for (let pIdx = 1; pIdx < layout.pages.length; pIdx++) {
        const seq = layout.pages[pIdx];
        slides.push({
          notes: speakerNotes,
          images: [],
          build(nextId, _imgs, pageLabel) {
            const out = buildSlideHeaderAndFooter(nextId, `${qTitle}（续${layout.pages.length > 2 ? pIdx : ''}）`, qSub, statBadge, footerLeft, pageLabel);
            out.push(...renderContentBlockSequence(nextId, seq, 0.55, contentTop, fullW, contentH, layout.fontSizePt, true));
            return out;
          }
        });
      }
    } else if (hasBottomImgs) {
      const textAreaH = contentH * 0.56;
      const imgAreaH = contentH * 0.41;
      const layout = layoutTextPages(textBlocks, fullW, textAreaH * 72);
      const firstSeq = layout.pages[0];
      // Place bottom images horizontally side by side.
      const colW = (fullW - 0.18 * (loadedImgs.length - 1)) / loadedImgs.length;
      const placedImgs = loadedImgs.flatMap((img, idx) => fitImagesInBox([img], 0.55 + idx * (colW + 0.18), contentTop + textAreaH + 0.18, colW, imgAreaH));
      slides.push({
        notes: speakerNotes,
        images: placedImgs.map(p => p.img),
        build(nextId, slideImages, pageLabel) {
          const out = buildSlideHeaderAndFooter(nextId, qTitle, qSub, statBadge, footerLeft, pageLabel);
          out.push(...renderContentBlockSequence(nextId, firstSeq, 0.55, contentTop, fullW, textAreaH, layout.fontSizePt, true));
          for (const p of placedImgs) {
            const relId = slideImages.get(p.img);
            out.push(renderPptPicture(nextId(), relId, p.x, p.y, p.w, p.h));
          }
          return out;
        }
      });
      for (let pIdx = 1; pIdx < layout.pages.length; pIdx++) {
        const seq = layout.pages[pIdx];
        slides.push({
          notes: speakerNotes,
          images: [],
          build(nextId, _imgs, pageLabel) {
            const out = buildSlideHeaderAndFooter(nextId, `${qTitle}（续${layout.pages.length > 2 ? pIdx : ''}）`, qSub, statBadge, footerLeft, pageLabel);
            out.push(...renderContentBlockSequence(nextId, seq, 0.55, contentTop, fullW, contentH, layout.fontSizePt, true));
            return out;
          }
        });
      }
    } else {
      const layout = layoutTextPages(textBlocks, fullW, contentH * 72);
      for (let pIdx = 0; pIdx < layout.pages.length; pIdx++) {
        const seq = layout.pages[pIdx];
        const title = pIdx === 0 ? qTitle : `${qTitle}（续${layout.pages.length > 2 ? pIdx : ''}）`;
        slides.push({
          notes: speakerNotes,
          images: [],
          build(nextId, _imgs, pageLabel) {
            const out = buildSlideHeaderAndFooter(nextId, title, qSub, statBadge, footerLeft, pageLabel);
            out.push(...renderContentBlockSequence(nextId, seq, 0.55, contentTop, fullW, contentH, layout.fontSizePt, true));
            return out;
          }
        });
      }
    }

    if (opts.answerMode === 'nextSlide') {
      const ansBlocks = [];
      if (ansText) {
        ansBlocks.push({ type: 'p', runs: [{ text: '【参考答案】', bold: true, color: '2F6B4F' }] });
        for (const b of htmlToPptBlocks(item.answerHtml || '')) {
          if (b.type !== 'img') ansBlocks.push(b);
        }
      }
      if (anaText) {
        ansBlocks.push({ type: 'p', runs: [{ text: '【解析】', bold: true, color: '2F6B4F' }] });
        for (const b of htmlToPptBlocks(item.analysisHtml || '')) {
          if (b.type !== 'img') ansBlocks.push(b);
        }
      }
      if (item.ruleNote) {
        ansBlocks.push({ type: 'p', runs: [{ text: '【评分细则】', bold: true, color: 'A5611A' }] });
        for (const line of String(item.ruleNote).split('\n').filter(Boolean)) {
          ansBlocks.push({ type: 'p', runs: [{ text: line }] });
        }
      }
      const solImgs = [];
      for (const b of [...htmlToPptBlocks(item.answerHtml || ''), ...htmlToPptBlocks(item.analysisHtml || '')]) {
        if (b.type === 'img') {
          try { solImgs.push(await registerImage(b.src)); } catch {}
        }
      }
      const ansW = solImgs.length ? 7.6 : fullW;
      const ansLayout = layoutTextPages(ansBlocks, ansW, contentH * 72);
      for (let pIdx = 0; pIdx < ansLayout.pages.length; pIdx++) {
        const seq = ansLayout.pages[pIdx];
        const useImgs = pIdx === 0 ? solImgs : [];
        const placedSol = useImgs.length ? fitImagesInBox(useImgs, 0.55 + ansW + 0.22, contentTop + 0.1, fullW - ansW - 0.22, contentH - 0.2) : [];
        slides.push({
          notes: opts.includeTeachingNotes && item.teachingNote ? '【教师讲评要点】\n' + item.teachingNote.trim() : speakerNotes,
          images: placedSol.map(p => p.img),
          build(nextId, slideImages, pageLabel) {
            const out = buildSlideHeaderAndFooter(nextId, `${qTitle} · 答案与解析${pIdx > 0 ? '（续）' : ''}`, qSub, statBadge, footerLeft, pageLabel);
            out.push(...renderContentBlockSequence(nextId, seq, 0.55, contentTop, useImgs.length ? ansW : fullW, contentH, ansLayout.fontSizePt, true));
            for (const p of placedSol) {
              out.push(renderPptPicture(nextId(), slideImages.get(p.img), p.x, p.y, p.w, p.h));
            }
            return out;
          }
        });
      }
    }
  }

  if (!slides.length) throw Error('没有可导出的题目。');

  // Static OOXML boilerplate: Theme, Masters, Layout, DocProps
  const extDefaults = Array.from(usedExts).map(ext => {
    const ct = ext === 'jpg' ? 'image/jpeg' : 'image/' + ext;
    return `<Default Extension="${ext}" ContentType="${ct}"/>`;
  }).join('');
  const slideOverrides = slides.map((s, i) => {
    const n = i + 1;
    return `<Override PartName="/ppt/slides/slide${n}.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>` +
      `<Override PartName="/ppt/notesSlides/notesSlide${n}.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.notesSlide+xml"/>`;
  }).join('');

  zip.file('[Content_Types].xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  ${extDefaults}
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  <Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
  <Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>
  <Override PartName="/ppt/notesMasters/notesMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.notesMaster+xml"/>
  <Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
  <Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>
  ${slideOverrides}
</Types>`);

  zip.file('_rels/.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>
</Relationships>`);

  const nowIso = new Date().toISOString().replace(/\.\d+Z$/, 'Z');
  zip.file('docProps/core.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <dc:title>${pptXmlEscape(edition.name || '生物试卷讲评')}</dc:title>
  <dc:creator>Biofolio</dc:creator>
  <dcterms:created xsi:type="dcterms:W3CDTF">${nowIso}</dcterms:created>
  <dcterms:modified xsi:type="dcterms:W3CDTF">${nowIso}</dcterms:modified>
</cp:coreProperties>`);

  zip.file('docProps/app.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties">
  <Application>Biofolio</Application>
  <Slides>${slides.length}</Slides>
  <ScaleCrop>false</ScaleCrop>
  <LinksUpToDate>false</LinksUpToDate>
  <SharedDoc>false</SharedDoc>
  <HyperlinksChanged>false</HyperlinksChanged>
</Properties>`);

  zip.file('ppt/theme/theme1.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Biofolio">
  <a:themeElements>
    <a:clrScheme name="Biofolio">
      <a:dk1><a:srgbClr val="1F2A24"/></a:dk1><a:lt1><a:srgbClr val="FFFFFF"/></a:lt1>
      <a:dk2><a:srgbClr val="2F6B4F"/></a:dk2><a:lt2><a:srgbClr val="F5F4EF"/></a:lt2>
      <a:accent1><a:srgbClr val="2F6B4F"/></a:accent1><a:accent2><a:srgbClr val="A5611A"/></a:accent2>
      <a:accent3><a:srgbClr val="5F7D6C"/></a:accent3><a:accent4><a:srgbClr val="274E6A"/></a:accent4>
      <a:accent5><a:srgbClr val="8AA596"/></a:accent5><a:accent6><a:srgbClr val="67716B"/></a:accent6>
      <a:hlink><a:srgbClr val="2F6B4F"/></a:hlink><a:folHlink><a:srgbClr val="5F7D6C"/></a:folHlink>
    </a:clrScheme>
    <a:fontScheme name="Biofolio">
      <a:majorFont><a:latin typeface="Calibri"/><a:ea typeface="微软雅黑"/><a:cs typeface="Calibri"/></a:majorFont>
      <a:minorFont><a:latin typeface="Calibri"/><a:ea typeface="微软雅黑"/><a:cs typeface="Calibri"/></a:minorFont>
    </a:fontScheme>
    <a:fmtScheme name="Biofolio">
      <a:fillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:fillStyleLst>
      <a:lnStyleLst><a:ln w="9525"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln><a:ln w="19050"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln><a:ln w="25400"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln></a:lnStyleLst>
      <a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle></a:effectStyleLst>
      <a:bgFillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:bgFillStyleLst>
    </a:fmtScheme>
  </a:themeElements>
</a:theme>`);

  zip.file('ppt/slideMasters/slideMaster1.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:bg><p:bgRef idx="1001"><a:schemeClr val="bg1"/></p:bgRef></p:bg><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld>
  <p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
  <p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst>
  <p:txStyles><p:titleStyle><a:lvl1pPr algn="l"><a:defRPr sz="3200"/></a:lvl1pPr></p:titleStyle><p:bodyStyle><a:lvl1pPr algn="l"><a:defRPr sz="1800"/></a:lvl1pPr></p:bodyStyle><p:otherStyle><a:defPPr><a:defRPr lang="zh-CN"/></a:defPPr></p:otherStyle></p:txStyles>
</p:sldMaster>`);

  zip.file('ppt/slideMasters/_rels/slideMaster1.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/>
</Relationships>`);

  zip.file('ppt/slideLayouts/slideLayout1.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="blank" preserve="1">
  <p:cSld name="Blank"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld>
  <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr>
</p:sldLayout>`);

  zip.file('ppt/slideLayouts/_rels/slideLayout1.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/>
</Relationships>`);

  zip.file('ppt/notesMasters/notesMaster1.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:notesMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:bg><p:bgRef idx="1001"><a:schemeClr val="bg1"/></p:bgRef></p:bg><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr></p:spTree></p:cSld>
  <p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
  <p:notesStyle><a:lvl1pPr><a:defRPr sz="1200"/></a:lvl1pPr></p:notesStyle>
</p:notesMaster>`);

  zip.file('ppt/notesMasters/_rels/notesMaster1.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/>
</Relationships>`);

  const presRels = [
    `<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>`,
    `<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesMaster" Target="notesMasters/notesMaster1.xml"/>`,
    `<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="theme/theme1.xml"/>`
  ];
  const sldIds = [];

  for (let i = 0; i < slides.length; i++) {
    const n = i + 1;
    const s = slides[i];
    const relId = `rId${3 + n}`;
    presRels.push(`<Relationship Id="${relId}" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide${n}.xml"/>`);
    sldIds.push(`<p:sldId id="${255 + n}" r:id="${relId}"/>`);

    let shapeSeq = 1;
    const nextId = () => ++shapeSeq;
    const slideImages = new Map();
    const sRels = [
      `<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`,
      `<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide" Target="../notesSlides/notesSlide${n}.xml"/>`
    ];
    const seenSlideImgs = [];
    for (const img of s.images || []) {
      if (!slideImages.has(img)) {
        const imgRid = `rId${3 + seenSlideImgs.length}`;
        slideImages.set(img, imgRid);
        seenSlideImgs.push(img);
        sRels.push(`<Relationship Id="${imgRid}" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/${img.filename}"/>`);
      }
    }
    const pageLabel = `${n} / ${slides.length}`;
    const shapesXml = s.build(nextId, slideImages, pageLabel).join('');
    zip.file(`ppt/slides/slide${n}.xml`, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:bg><p:bgPr><a:solidFill><a:srgbClr val="FBFBF8"/></a:solidFill><a:effectLst/></p:bgPr></p:bg><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>${shapesXml}</p:spTree></p:cSld>
  <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr>
</p:sld>`);
    zip.file(`ppt/slides/_rels/slide${n}.xml.rels`, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">${sRels.join('')}</Relationships>`);

    const noteParas = (s.notes || '').split(/\r?\n/).map(line =>
      `<a:p><a:r><a:rPr lang="zh-CN" sz="1200"/><a:t>${pptXmlEscape(line)}</a:t></a:r><a:endParaRPr lang="zh-CN" sz="1200"/></a:p>`
    ).join('') || '<a:p><a:endParaRPr lang="zh-CN" sz="1200"/></a:p>';
    zip.file(`ppt/notesSlides/notesSlide${n}.xml`, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:notes xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>
    <p:sp><p:nvSpPr><p:cNvPr id="2" name="Slide Image"/><p:cNvSpPr><a:spLocks noGrp="1" noRot="1" noChangeAspect="1"/></p:cNvSpPr><p:nvPr><p:ph type="sldImg"/></p:nvPr></p:nvSpPr><p:spPr><a:xfrm><a:off x="685800" y="685800"/><a:ext cx="5486400" cy="3086100"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr></p:sp>
    <p:sp><p:nvSpPr><p:cNvPr id="3" name="Notes"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr><p:ph type="body" idx="1"/></p:nvPr></p:nvSpPr><p:spPr><a:xfrm><a:off x="685800" y="4114800"/><a:ext cx="5486400" cy="4343400"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr><p:txBody><a:bodyPr/><a:lstStyle/>${noteParas}</p:txBody></p:sp>
  </p:spTree></p:cSld>
  <p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr>
</p:notes>`);
    zip.file(`ppt/notesSlides/_rels/notesSlide${n}.xml.rels`, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesMaster" Target="../notesMasters/notesMaster1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="../slides/slide${n}.xml"/>
</Relationships>`);
  }

  zip.file('ppt/presentation.xml', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>
  <p:notesMasterIdLst><p:notesMasterId r:id="rId2"/></p:notesMasterIdLst>
  <p:sldIdLst>${sldIds.join('')}</p:sldIdLst>
  <p:sldSz cx="${pptIn(PPT_W_IN)}" cy="${pptIn(PPT_H_IN)}"/>
  <p:notesSz cx="6858000" cy="9144000"/>
  <p:defaultTextStyle><a:defPPr><a:defRPr lang="zh-CN"/></a:defPPr></p:defaultTextStyle>
</p:presentation>`);
  zip.file('ppt/_rels/presentation.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">${presRels.join('')}</Relationships>`);

  if (typeof Blob !== 'undefined') {
    return zip.generateAsync({ type: 'blob', compression: 'DEFLATE', mimeType: 'application/vnd.openxmlformats-officedocument.presentationml.presentation' });
  }
  return zip.generateAsync({ type: 'uint8array', compression: 'DEFLATE' });
}

// Inline library images inside sanitized HTML for standalone A4 print / PDF export.
async function inlineHtmlPictures(html) {
  if (!html || !html.includes('library/picture/')) return safeHtml(html || '');
  const tpl = document.createElement('template');
  tpl.innerHTML = safeHtml(html);
  for (const img of tpl.content.querySelectorAll('img')) {
    const src = img.getAttribute('src');
    if (src && libraryPicture(src)) img.setAttribute('src', await inlinePicture(src));
  }
  return tpl.innerHTML;
}

async function buildEditionPrintHtml(edition, items, mode) {
  const teacher = mode === 'teacher';
  const grading = edition.grading;
  const parts = [];
  parts.push(`<header class="paper-head"><h1>${escapeDoc(edition.name || '生物试卷')}</h1>`);
  const meta = [edition.purpose, grading ? `满分 ${grading.total} 分` : '', `${items.length} 道题`].filter(Boolean).join('　·　');
  if (meta) parts.push(`<p class="paper-meta">${escapeDoc(meta)}</p>`);
  if (mode === 'student') {
    parts.push(`<p class="student-line">班级：______________　姓名：______________　学号：______________</p>`);
  }
  parts.push('</header>');

  if (mode === 'answers') {
    parts.push('<h2>参考答案及评分标准</h2>');
    const choice = [], rest = [];
    for (const [i, item] of items.entries()) {
      const kind = basketSection(item.q), short = kind < 3 ? shortAnswer(item.answer) : '';
      (short ? choice : rest).push({ n: i + 1, item, short });
    }
    if (choice.length) {
      parts.push('<h3>一、选择题</h3>');
      for (let s = 0; s < choice.length; s += 10) {
        const row = choice.slice(s, s + 10);
        const pad = Array(10 - row.length).fill('');
        parts.push('<table class="ans-grid"><tr><th>题号</th>' + row.map(r => `<th>${r.n}</th>`).join('') + pad.map(() => '<th></th>').join('') +
          '</tr><tr><td>答案</td>' + row.map(r => `<td>${escapeDoc(r.short)}</td>`).join('') + pad.map(() => '<td></td>').join('') + '</tr></table>');
      }
      const kinds = new Set(choice.map(r => basketSection(r.item.q)));
      if (grading) for (const kind of [1, 2]) if (kinds.has(kind)) {
        const id = choice.find(r => basketSection(r.item.q) === kind).item.q.id;
        parts.push(`<p class="rule-line"><strong>${kind === 1 ? '单项选择题：' : '多项选择题：'}</strong>${escapeDoc(markScheme(grading, id))}</p>`);
      }
    }
    if (rest.length) {
      parts.push(`<h3>${choice.length ? '二、' : ''}非选择题</h3>`);
      for (const { n, item } of rest) {
        const rule = grading?.items[item.q.id];
        const ans = await inlineHtmlPictures(item.answer);
        parts.push(`<article class="q-item"><div class="q-title"><strong>${n}．</strong>${rule?.kind === 3 ? `（${rule.points}分）` : ''}</div><div class="doc">${ans}</div>`);
        if (rule) {
          parts.push(`<p class="rule-line"><strong>【评分细则】</strong>${escapeDoc(markScheme(grading, item.q.id))}${rule.note ? '<br>' + escapeDoc(rule.note).replaceAll('\n', '<br>') : ''}</p>`);
        }
        parts.push('</article>');
      }
    }
  } else {
    const headings = new Map(paperSections(items.map(it => it.q), grading).map(g => [g.questions[0].id, g.title]));
    for (const [i, item] of items.entries()) {
      if (headings.has(item.q.id)) parts.push(`<h2 class="section-head">${escapeDoc(headings.get(item.q.id))}</h2>`);
      const rule = grading?.items[item.q.id];
      const rawFace = basketSection(item.q) < 3
        ? blankPrintedAnswer(scoredFace(item.q, grading), shortAnswer(item.answer))
        : scoredFace(item.q, grading);
      const face = await inlineHtmlPictures(rawFace);
      const prefix = `${i + 1}．` + (rule?.kind === 3 ? `（${rule.points}分）` : '');
      parts.push(`<article class="q-item"><div class="q-body"><span class="q-no">${escapeDoc(prefix)}</span><div class="doc">${face}</div></div>`);
      if (teacher) {
        const ans = await inlineHtmlPictures(item.answer);
        const ana = await inlineHtmlPictures(item.analysis);
        parts.push(`<div class="sol-box"><div class="sol-row"><strong>【答案】</strong><div class="doc">${ans}</div></div><div class="sol-row"><strong>【解析】</strong><div class="doc">${ana}</div></div>`);
        if (rule) parts.push(`<p class="rule-line"><strong>【评分细则】</strong>${escapeDoc(markScheme(grading, item.q.id))}${rule.note ? '<br>' + escapeDoc(rule.note).replaceAll('\n', '<br>') : ''}</p>`);
        if (item.source) parts.push(`<p class="src-line">【来源】${escapeDoc(item.source)}</p>`);
        parts.push('</div>');
      }
      parts.push('</article>');
    }
  }

  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:;">
<title>${escapeDoc(edition.name || '生物试卷')}</title>
<style>
@page { size: A4; margin: 16mm 18mm; }
* { box-sizing: border-box; }
body { margin: 0; color: #111; background: #fff; font: 10.5pt/1.65 "Songti SC", "SimSun", "Noto Serif CJK SC", "Times New Roman", serif; }
.paper-head { text-align: center; margin-bottom: 14pt; padding-bottom: 8pt; border-bottom: 1pt solid #bbb; }
.paper-head h1 { margin: 0 0 4pt; font-size: 16pt; font-weight: 700; }
.paper-meta, .student-line { margin: 3pt 0; font-size: 9.5pt; color: #333; }
.section-head { margin: 14pt 0 8pt; font-size: 12pt; font-weight: 700; font-family: "Kaiti SC", "KaiTi", "STKaiti", serif; break-after: avoid; }
h2 { font-size: 13pt; margin: 12pt 0 8pt; }
h3 { font-size: 11pt; margin: 10pt 0 6pt; break-after: avoid; }
.q-item { margin: 0 0 11pt; break-inside: avoid; }
.q-body { display: block; }
.q-no { font-weight: 600; float: left; margin-right: 4pt; }
.doc p { margin: 0 0 4pt; white-space: pre-wrap; }
.doc p:first-child { display: inline; }
.doc p:first-child::after { content: ""; display: block; }
.doc img { display: block; max-width: 100%; max-height: 110mm; height: auto; margin: 5pt 0; object-fit: contain; }
.doc table, .ans-grid { border-collapse: collapse; margin: 6pt 0; max-width: 100%; }
.doc td, .doc th, .ans-grid td, .ans-grid th { border: .6pt solid #444; padding: 3pt 7pt; font-size: 9.5pt; }
.ans-grid th { background: #f2f4f2; text-align: center; }
.ans-grid td { text-align: center; min-width: 24pt; }
.sol-box { margin: 6pt 0 2pt 14pt; padding: 6pt 9pt; border-left: 2pt solid #2f6b4f; background: #f9faf8; font-size: 9.5pt; }
.sol-row { margin: 3pt 0; }
.rule-line, .src-line { margin: 4pt 0; font-size: 9pt; color: #333; }
</style></head><body>${parts.join('')}</body></html>`;
}

async function printEditionPdf(edition, items, mode) {
  const html = await buildEditionPrintHtml(edition, items, mode);
  const frame = document.createElement('iframe');
  frame.setAttribute('aria-hidden', 'true');
  frame.style.cssText = 'position:fixed;right:0;bottom:0;width:0;height:0;border:0;opacity:0;pointer-events:none';
  document.body.appendChild(frame);
  const cleanup = () => { if (frame.parentNode) frame.remove(); };
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(Error('打印预览准备超时，请重试。')), 15000);
    frame.onload = async () => {
      clearTimeout(timer);
      try {
        const imgs = Array.from(frame.contentDocument?.images || []);
        await Promise.all(imgs.map(img => img.complete ? Promise.resolve() : new Promise(r => { img.onload = r; img.onerror = r; })));
        resolve();
      } catch (e) { reject(e); }
    };
    frame.srcdoc = html;
  });
  frame.contentWindow.addEventListener('afterprint', cleanup, { once: true });
  setTimeout(cleanup, 60000);
  frame.contentWindow.focus();
  frame.contentWindow.print();
}

const pptAndPresenter = {
  data() {
    return {
      pptTargetEdition: null,
      pptScope: 'all',         // 'all' | 'marked' | 'threshold'
      pptThreshold: 40,
      pptAnswerMode: 'notes',  // 'notes' | 'nextSlide' | 'none'
      pptIncludeCover: true,
      pptShowStats: true,
      pptShowSource: true,
      pptIncludeTeachingNotes: true,
      pptBusy: false,
      pptNotice: '',
      presentId: '',
      presentIndex: 0,
      presentOnlyWrong: false,
      presentShowAnswer: false,
      presentShowTeaching: true,
      presentFontScale: 1
    };
  },
  computed: {
    pptPreviewItems() {
      const ed = this.pptTargetEdition;
      if (!ed) return [];
      return this.filterEditionItemsForPresentation(ed, this.pptScope, this.pptThreshold);
    },
    presentEdition() {
      return this.editions.find(e => e.id === this.presentId) || null;
    },
    presentItems() {
      const ed = this.presentEdition;
      if (!ed) return [];
      const items = this.filterEditionItemsForPresentation(ed, this.presentOnlyWrong ? 'marked' : 'all', 40);
      return items.length ? items : this.filterEditionItemsForPresentation(ed, 'all', 40);
    },
    presentCurrent() {
      if (!this.presentItems.length) return null;
      const idx = Math.max(0, Math.min(this.presentItems.length - 1, this.presentIndex));
      return this.presentItems[idx];
    }
  },
  mounted() {
    this._onPresentKey = event => {
      if (this.page !== '投屏讲题' || document.querySelector('dialog[open]')) return;
      if (event.key === 'ArrowRight' || event.key === 'PageDown') { event.preventDefault(); this.stepPresenter(1); }
      else if (event.key === 'ArrowLeft' || event.key === 'PageUp') { event.preventDefault(); this.stepPresenter(-1); }
      else if (event.key === ' ' || event.key === 'Enter') {
        if (['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON'].includes(document.activeElement?.tagName)) return;
        event.preventDefault(); this.presentShowAnswer = !this.presentShowAnswer;
      } else if (event.key === 'Escape' && !document.fullscreenElement) {
        this.exitPresenter();
      }
    };
    window.addEventListener('keydown', this._onPresentKey);
  },
  beforeUnmount() {
    if (this._onPresentKey) window.removeEventListener('keydown', this._onPresentKey);
  },
  methods: {
    editionStatOf(edition, n, qId) {
      const row = (edition?.stats || []).find(s => s.n === n);
      if (!row) return null;
      const rate = row.lossRate != null ? row.lossRate : (row.total ? row.missed / row.total : null);
      let text = '';
      if (row.lossRate != null && row.max) {
        const mean = Math.max(0, (1 - row.lossRate) * row.max);
        text = `失分率 ${Math.round(row.lossRate * 100)}% · 均分 ${Number(mean.toFixed(1))}/${row.max}`;
      } else if (row.total) {
        text = `错误率 ${Math.round((row.missed / row.total) * 100)}%（${row.missed}/${row.total}）`;
      } else {
        text = `${row.missed} 人失分`;
      }
      return { row, rate, text, hot: (rate ?? 0) >= 0.4 || (edition.marked || []).includes(qId) };
    },
    filterEditionItemsForPresentation(edition, scope, threshold) {
      const marked = new Set(edition.marked || []);
      const statsMap = new Map((edition.stats || []).map(s => [s.n, s]));
      const out = [];
      for (let i = 0; i < (edition.ids || []).length; i++) {
        const id = edition.ids[i], n = i + 1;
        const q = this.editionQuestion(id, edition);
        if (!q) continue;
        const stat = this.editionStatOf(edition, n, id);
        if (scope === 'marked' && !marked.has(id)) continue;
        if (scope === 'threshold') {
          const r = stat?.rate;
          if (r == null || r * 100 + 1e-9 < threshold) continue;
        }
        const fix = edition.fixes?.[id] || this.fixes[id] || {};
        const grading = edition.grading;
        const rule = grading?.items?.[id];
        const rawFace = basketSection(q) < 3
          ? blankPrintedAnswer(scoredFace(q, grading), shortAnswer(q.answerHtml || plainBlocks(fix.answer || q.answer)))
          : scoredFace(q, grading);
        const typeLabel = q.type === '非选择题' ? primaryType(q) : q.type;
        const ruleNote = rule ? (markScheme(grading, id) + (rule.note ? '\n' + rule.note : '')) : '';
        out.push({
          q, n, id, typeLabel,
          points: rule?.points || 0,
          faceHtml: rawFace,
          answerHtml: q.answerHtml || plainBlocks(fix.answer || q.answer || ''),
          analysisHtml: q.analysisHtml || plainBlocks(fix.analysis || q.a || ''),
          source: fix.source ?? q.origin ?? '',
          teachingNote: fix.teachingNote || '',
          ruleNote,
          stat: statsMap.get(n) || null,
          statText: stat?.text || '',
          statHot: !!stat?.hot,
          marked: marked.has(id)
        });
      }
      return out;
    },
    async openPptDialog(edition) {
      try { await this.ensureQuestions(); } catch (e) { this.notify(e.message); return; }
      this.pptTargetEdition = edition;
      this.pptNotice = '';
      const hasMarked = (edition.marked || []).length > 0;
      const hasRates = (edition.stats || []).some(s => s.lossRate != null || s.total > 0);
      this.pptScope = hasMarked ? 'marked' : hasRates ? 'threshold' : 'all';
      this.$refs.pptDialog?.showModal();
    },
    async exportEditionPpt() {
      const edition = this.pptTargetEdition;
      if (!edition || this.pptBusy) return;
      const items = this.pptPreviewItems;
      if (!items.length) { this.pptNotice = '当前筛选条件下没有题目，请切换为「全部题目」或调低阈值。'; return; }
      this.pptBusy = true;
      this.pptNotice = '正在排版并生成课件…';
      try {
        const blob = await makeEditionPptx(edition, items, {
          answerMode: this.pptAnswerMode,
          includeCover: this.pptIncludeCover,
          showStats: this.pptShowStats,
          showSource: this.pptShowSource,
          includeTeachingNotes: this.pptIncludeTeachingNotes
        });
        const suffix = this.pptScope === 'all' ? '讲评课件' : '错题讲评课件';
        const url = URL.createObjectURL(blob), a = document.createElement('a');
        a.href = url;
        a.download = `${(edition.name || '生物试卷').replace(/[\\/:*?"<>|]/g, '_')}（${suffix}）.pptx`;
        a.click();
        setTimeout(() => URL.revokeObjectURL(url), 30000);
        this.pptNotice = `已导出 ${items.length} 道题的讲评 PPT。`;
        this.$refs.pptDialog?.close();
        this.notify(this.pptNotice);
      } catch (e) {
        this.pptNotice = '导出失败：' + e.message;
      } finally {
        this.pptBusy = false;
      }
    },
    async printEdition(edition, mode) {
      if (this.wordBusy) return;
      this.wordBusy = true;
      this.wordNotice = { id: edition.id, text: '正在准备打印 / PDF 排版…' };
      try {
        await this.ensureQuestions();
        const items = edition.ids.map((id, i) => {
          const q = this.editionQuestion(id, edition);
          if (q && this.conflictingLinks(q).length) throw Error('第 ' + (i + 1) + ' 题存在答案冲突，请先核对。');
          if (!q) throw Error('第 ' + (i + 1) + ' 题尚未载入，请稍后再试。');
          const fix = edition.fixes?.[id] || this.fixes[id] || {};
          const answer = q.answerHtml || plainBlocks(fix.answer || q.answer);
          const analysis = q.analysisHtml || plainBlocks(fix.analysis || q.a);
          return { q, answer, analysis, source: fix.source ?? q.origin };
        });
        await printEditionPdf(edition, items, mode);
        this.wordNotice = {};
      } catch (e) {
        this.wordNotice = { id: edition.id, text: e.message };
      } finally {
        this.wordBusy = false;
      }
    },
    startPresenter(edition) {
      this.presentIndex = 0;
      this.presentShowAnswer = false;
      this.presentOnlyWrong = (edition.marked || []).length > 0;
      location.hash = 'present/' + encodeURIComponent(edition.id);
    },
    stepPresenter(delta) {
      if (!this.presentItems.length) return;
      const next = Math.max(0, Math.min(this.presentItems.length - 1, this.presentIndex + delta));
      if (next !== this.presentIndex) {
        this.presentIndex = next;
        this.presentShowAnswer = false;
      }
    },
    togglePresenterFullscreen() {
      if (document.fullscreenElement) document.exitFullscreen?.();
      else document.documentElement.requestFullscreen?.().catch(() => {});
    },
    exitPresenter() {
      if (document.fullscreenElement) document.exitFullscreen?.();
      const target = this.presentId ? 'review/' + encodeURIComponent(this.presentId) : 'saved';
      location.hash = target;
    },
    closePresenter() {
      if (document.fullscreenElement) document.exitFullscreen?.();
    }
  }
};
