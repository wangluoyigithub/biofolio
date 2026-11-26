// Grading pack JSON v2.1 export, deterministic paper_id, 3-tier image bounding box
// (choice 95x48mm / subjective 140x65mm / subjective-lg 165x85mm + 1.25x@150DPI cap +
// tall-stack h/w>1.35 -> 105x85mm), 180mm 10-col objective answer grid (18mm wide,
// 5/5/10mm rows) with single BFGRID0 anchor, 15mm vector footer QR (pdf-lib drawRectangle,
// for 260x368 正度 8K @ 300 DPI), single-section grading Word (.docx), and one-click
// .docx/.pdf sealing with BFGRID0 missing safety blocker.
const BUILTIN_GRADING_FORMAT_ID = '__grading__';
const DEFAULT_STUDENT_ID_DIGITS = 4;
const GRADING_SCHEMA_VERSION = '2.1';
const GRADING_QR_SIZE_MM = 15; // 15mm on A4 -> ~13.1mm after 87.5% shrink onto 260x368mm 正度 8K (>= 13mm @ 300 DPI)
const GRADING_SCAN_DPI = 300;
const GRADING_ANCHOR_SIZE_MM = 4;
const GRADING_ANCHOR_INSET_MM = 6;
const GRADING_ANCHOR_TL_CENTER_MM = [8.0, 8.0];
const GRADING_ANCHOR_BR_CENTER_MM = [202.0, 289.0];
const GRADING_ANCHOR_REF_W_MM = 194.0;
const GRADING_ANCHOR_REF_H_MM = 281.0;
const GRADING_PAGE_MARGIN_TOP_MM = 14;
const GRADING_PAGE_MARGIN_BOTTOM_MM = 22;
const GRADING_PAGE_MARGIN_SIDE_MM = 15;
const GRADING_BODY_WIDTH_MM = 180;   // 210 - 15 * 2
const GRADING_BODY_HEIGHT_MM = 261;  // 297 - 14 - 22
const GRADING_FOOTER_QR_BOTTOM_MM = 5.0;
const GRADING_FOOTER_QR_X_MM = 180.0; // 210 - 15 - 15 (right-aligned to 180mm body)
const GRADING_FOOTER_QR_Y_MM = 277.0; // 297 - 5.0 - 15

const GRADING_IMG_TIER_LIMITS = {
  choice: { maxW: 95, maxH: 48 },
  subjective: { maxW: 140, maxH: 65 },
  'subjective-lg': { maxW: 165, maxH: 85 }
};
const GRADING_TALL_ASPECT_THRESHOLD = 1.35; // natH / natW > 1.35 vertical-stack diagram exemption
const GRADING_TALL_IMG_MAX_W_MM = 105;
const GRADING_TALL_IMG_MAX_H_MM = 85;
const GRADING_OBJ_IMG_MAX_W_MM = GRADING_IMG_TIER_LIMITS.choice.maxW;
const GRADING_OBJ_IMG_MAX_H_MM = GRADING_IMG_TIER_LIMITS.choice.maxH;
const GRADING_SUBJ_IMG_MAX_W_MM = GRADING_IMG_TIER_LIMITS.subjective.maxW;
const GRADING_SUBJ_IMG_MAX_H_MM = GRADING_IMG_TIER_LIMITS.subjective.maxH;
const GRADING_IMG_MAX_WIDTH_MM = GRADING_IMG_TIER_LIMITS['subjective-lg'].maxW;
const GRADING_IMG_MAX_HEIGHT_MM = GRADING_IMG_TIER_LIMITS['subjective-lg'].maxH;

const EMU_PER_MM = 36000;
const EMU_PER_PX96 = 9525; // docx.js multiplies transformation.{width,height} by 9525 EMU

const CHOICE_GRID_COLS = 10;
const CHOICE_GRID_CELL_W_MM = 18;
const CHOICE_GRID_TITLE_H_MM = 5;
const CHOICE_GRID_HEADER_H_MM = 5;
const CHOICE_GRID_WRITE_H_MM = 10;
const GRID_ANCHOR_TOKEN = 'BFGRID0';
// Calibrated baseline-to-row-top offset for BFGRID0 inside the 5.0mm exact Title Row
const GRID_ANCHOR_BASELINE_FROM_TOP_MM = 4.15;

function normalizeStudentIdDigits(value) {
  const n = Math.round(Number(value));
  if (!Number.isFinite(n) || n < 1) return DEFAULT_STUDENT_ID_DIGITS;
  return Math.min(16, Math.max(1, n));
}

function isGradingFormatId(formatId, gradingFormats = []) {
  return formatId === BUILTIN_GRADING_FORMAT_ID ||
    (Boolean(formatId) && Array.isArray(gradingFormats) && gradingFormats.includes(formatId));
}

function formatQrPayload(paperId, pageNo) {
  return 'BF:' + paperId + ':' + pageNo;
}

function abbreviatePaperTitle(title, maxLen = 12) {
  const clean = String(title || '生物试卷').replace(/\s+/g, ' ').trim();
  if (clean.length <= maxLen) return clean;
  return clean.slice(0, Math.max(1, maxLen - 1)) + '…';
}

function isoLocalTimestamp(date = new Date()) {
  const pad = n => String(n).padStart(2, '0');
  const tzMin = -date.getTimezoneOffset();
  const sign = tzMin >= 0 ? '+' : '-';
  const abs = Math.abs(tzMin);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

// ---------- Three-Tier Image Bounding Box (choice / subjective / subjective-lg) ----------

function questionImageTier(edition, q, isSubjective = false) {
  const id = typeof q === 'string' ? q : q?.id;
  const manual = id && edition?.imgTiers ? edition.imgTiers[id] : null;
  if (manual === 'choice' || manual === 'subjective' || manual === 'subjective-lg') {
    return { tier: manual, manual: true };
  }
  return { tier: isSubjective ? 'subjective' : 'choice', manual: false };
}

function decodeDataUrlBytes(dataUrl) {
  if (typeof dataUrl !== 'string' || !dataUrl.startsWith('data:')) return null;
  const comma = dataUrl.indexOf(',');
  if (comma < 0) return null;
  const b64 = dataUrl.slice(comma + 1);
  if (typeof atob === 'function') {
    const bin = atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }
  if (typeof Buffer !== 'undefined') {
    return new Uint8Array(Buffer.from(b64, 'base64'));
  }
  return null;
}

// Reads embedded physical resolution (PNG pHYs or JPEG JFIF) for tiny inline symbols/formulas.
function parseImagePhysicalDpi(bytes) {
  if (!bytes || bytes.length < 24) return null;
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);

  // PNG: 89 50 4E 47 0D 0A 1A 0A
  if (bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47) {
    let off = 8;
    while (off + 12 <= bytes.length) {
      const len = dv.getUint32(off, false);
      if (off + 12 + len > bytes.length) break;
      const type = String.fromCharCode(bytes[off + 4], bytes[off + 5], bytes[off + 6], bytes[off + 7]);
      if (type === 'pHYs' && len === 9) {
        const ppuX = dv.getUint32(off + 8, false);
        const ppuY = dv.getUint32(off + 12, false);
        const unit = bytes[off + 16];
        if (unit === 1 && ppuX > 0 && ppuY > 0) {
          const dpiX = ppuX * 0.0254;
          const dpiY = ppuY * 0.0254;
          if (dpiX >= 96.5 && dpiX <= 1200 && dpiY >= 96.5 && dpiY <= 1200) {
            return { dpiX, dpiY };
          }
        }
        return null;
      }
      if (type === 'IDAT' || type === 'IEND') break;
      off += 12 + len;
    }
    return null;
  }

  // JPEG: FF D8
  if (bytes[0] === 0xff && bytes[1] === 0xd8) {
    let i = 2;
    while (i + 4 <= bytes.length) {
      if (bytes[i] !== 0xff) { i++; continue; }
      while (i < bytes.length && bytes[i] === 0xff) i++;
      if (i >= bytes.length) break;
      const marker = bytes[i++];
      if (marker === 0xd9 || marker === 0xda) break;
      if (marker === 0xd8 || marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
      if (i + 2 > bytes.length) break;
      const segLen = (bytes[i] << 8) | bytes[i + 1];
      if (segLen < 2 || i + segLen > bytes.length) break;
      if (marker === 0xe0 && segLen >= 16 &&
          bytes[i + 2] === 0x4a && bytes[i + 3] === 0x46 && bytes[i + 4] === 0x49 && bytes[i + 5] === 0x46 && bytes[i + 6] === 0x00) {
        const units = bytes[i + 9];
        const xd = (bytes[i + 10] << 8) | bytes[i + 11];
        const yd = (bytes[i + 12] << 8) | bytes[i + 13];
        if ((units === 1 || units === 2) && xd > 0 && yd > 0) {
          const factor = units === 2 ? 2.54 : 1;
          const dpiX = xd * factor;
          const dpiY = yd * factor;
          if (dpiX >= 96.5 && dpiX <= 1200 && dpiY >= 96.5 && dpiY <= 1200) {
            return { dpiX, dpiY };
          }
        }
      }
      i += segLen;
    }
  }
  return null;
}

// Unified 3-tier image bounding-box pure function shared by PDF and Word (.docx) pipelines:
// - choice: 95 x 48 mm
// - subjective: 140 x 65 mm
// - subjective-lg: 165 x 85 mm
// - Small images capped at max 1.25x upscale assuming 150 DPI natural size (prevents stretching low-res clips)
// - Tall vertical-stack diagrams (natH / natW > 1.35) auto-relax maxH to 85mm and maxW to 105mm (unless manually set to 'choice')
function computeImageBoxMm(natPxW, natPxH, tier = 'choice', opts = {}) {
  const wPx = Math.max(1, Number(natPxW) || 320);
  const hPx = Math.max(1, Number(natPxH) || 240);
  const normTier = GRADING_IMG_TIER_LIMITS[tier] ? tier : (opts.isSubjective ? 'subjective' : 'choice');
  const limits = GRADING_IMG_TIER_LIMITS[normTier];
  let maxW = opts.maxWidthMm ?? limits.maxW;
  let maxH = opts.maxHeightMm ?? limits.maxH;

  const tallStack = (hPx / wPx) > GRADING_TALL_ASPECT_THRESHOLD;
  if (tallStack && opts.maxWidthMm == null && opts.maxHeightMm == null && !(opts.manual && normTier === 'choice')) {
    maxH = Math.max(maxH, GRADING_TALL_IMG_MAX_H_MM);
    maxW = normTier === 'subjective-lg' ? limits.maxW : GRADING_TALL_IMG_MAX_W_MM;
  }

  // Respect embedded physical DPI (>= 140 DPI, e.g. 300/600 DPI textbook figures) when no manual tier override is set,
  // while capping untagged/low-res images at max 1.25x upscale @ 150 DPI so small screenshots are never stretched blurry.
  const useEmbeddedDpi = Boolean(!opts.manual && opts.hasPhysicalDpi && (opts.dpiX || 0) >= 140);
  const baseW = useEmbeddedDpi
    ? (wPx / opts.dpiX) * 25.4 * 1.08
    : (wPx / 150) * 25.4 * 1.25;
  const baseH = useEmbeddedDpi
    ? (hPx / (opts.dpiY || opts.dpiX)) * 25.4 * 1.08
    : (hPx / 150) * 25.4 * 1.25;

  const scale = Math.min(maxW / baseW, maxH / baseH, 1);
  const widthMm = +(baseW * scale).toFixed(2);
  const heightMm = +(baseH * scale).toFixed(2);
  const widthEmu = Math.max(EMU_PER_PX96, Math.round(widthMm * EMU_PER_MM));
  const heightEmu = Math.max(EMU_PER_PX96, Math.round(heightMm * EMU_PER_MM));
  const widthPx = Math.max(1, Math.round(widthMm * (96 / 25.4)));
  const heightPx = Math.max(1, Math.round(heightMm * (96 / 25.4)));

  return {
    tier: normTier,
    tallStack,
    maxWidthMm: maxW,
    maxHeightMm: maxH,
    w_mm: widthMm,
    h_mm: heightMm,
    widthMm,
    heightMm,
    widthPx,
    heightPx,
    docxPxW: widthPx,
    docxPxH: heightPx,
    widthEmu,
    heightEmu,
    emuW: widthEmu,
    emuH: heightEmu,
    cxEmu: widthEmu,
    cyEmu: heightEmu,
    widthPx96: widthEmu / EMU_PER_PX96,
    heightPx96: heightEmu / EMU_PER_PX96,
    dpiX: opts.dpiX || 150,
    dpiY: opts.dpiY || 150,
    hasPhysicalDpi: Boolean(opts.hasPhysicalDpi)
  };
}

function calcBoundedImageSize(natW, natH, isSubjectiveOrOpts = false) {
  const opts = (typeof isSubjectiveOrOpts === 'boolean')
    ? { isSubjective: isSubjectiveOrOpts }
    : (isSubjectiveOrOpts || {});
  const tier = opts.tier || (opts.isSubjective ? 'subjective' : 'choice');
  return computeImageBoxMm(natW, natH, tier, opts);
}

function normalizeImageBox(origW, origH, opts = {}) {
  return calcBoundedImageSize(origW, origH, opts);
}

function computePrintImageBox(bytes, naturalWidth, naturalHeight, opts = {}) {
  const dpi = parseImagePhysicalDpi(bytes);
  return calcBoundedImageSize(naturalWidth, naturalHeight, {
    ...opts,
    dpiX: dpi?.dpiX || 150,
    dpiY: dpi?.dpiY || 150,
    hasPhysicalDpi: Boolean(dpi)
  });
}

async function resolvePrintImageAsset(src, opts = {}) {
  let url = await inlinePicture(src);
  if (!url || !url.startsWith('data:image/')) throw Error('配图无法读取');
  let bytes = decodeDataUrlBytes(url);
  let naturalWidth = 0, naturalHeight = 0;
  let imgEl = null;
  if (typeof Image !== 'undefined' && typeof document !== 'undefined') {
    imgEl = new Image();
    await new Promise((resolve, reject) => {
      imgEl.onload = resolve;
      imgEl.onerror = () => reject(Error('配图无法读取'));
      imgEl.src = url;
      if (imgEl.complete && imgEl.naturalWidth) resolve();
    });
    naturalWidth = imgEl.naturalWidth;
    naturalHeight = imgEl.naturalHeight;
  }
  let type = url.match(/^data:image\/(png|jpeg|jpg|gif|bmp);/i)?.[1]?.toLowerCase();
  if (!type && imgEl && typeof document !== 'undefined') {
    const canvas = document.createElement('canvas');
    canvas.width = naturalWidth || 1;
    canvas.height = naturalHeight || 1;
    canvas.getContext('2d').drawImage(imgEl, 0, 0);
    url = canvas.toDataURL('image/png');
    type = 'png';
    bytes = decodeDataUrlBytes(url);
  }
  if ((!naturalWidth || !naturalHeight) && typeof pptImageDimensions === 'function' && bytes) {
    const dim = pptImageDimensions(bytes, 'image/' + (type || 'png'));
    naturalWidth = dim.width;
    naturalHeight = dim.height;
  }
  const box = computePrintImageBox(bytes, naturalWidth, naturalHeight, opts);
  return {
    url,
    type: (type === 'jpeg' || type === 'jpg') ? 'jpg' : (type || 'png'),
    bytes,
    naturalWidth,
    naturalHeight,
    ...box
  };
}

// ---------- Paper Fingerprint & Grading Pack JSON v2.1 ----------

function gradingScoreFingerprint(grading) {
  if (!grading) return null;
  const itemKeys = Object.keys(grading.items || {}).sort();
  const items = {};
  for (const id of itemKeys) {
    const r = grading.items[id];
    if (!r) continue;
    items[id] = {
      kind: r.kind,
      points: r.points,
      parts: Array.isArray(r.parts) ? r.parts.slice() : [],
      ...(r.isDrawing ? { isDrawing: true } : {})
    };
  }
  return {
    judgment: grading.judgment ?? 1,
    single: grading.single ?? 2,
    multi: grading.multi ?? 3,
    partial: grading.partial ?? 1,
    total: grading.total ?? 0,
    items
  };
}

async function computePaperId(edition) {
  const ids = Array.isArray(edition?.ids) ? edition.ids : [];
  const versions = {};
  for (const id of ids) versions[id] = edition?.versions?.[id] || 0;
  const canonical = JSON.stringify({
    v: 1,
    editionId: edition?.id || '',
    ids,
    versions,
    grading: gradingScoreFingerprint(edition?.grading || null)
  });
  const digest = await fileDigest(new TextEncoder().encode(canonical));
  return digest.slice(0, 10).toUpperCase();
}

function htmlToPlainText(html) {
  if (!html) return '';
  if (typeof document === 'undefined') {
    return String(html).replace(/<br\s*\/?>/gi, '\n').replace(/<\/(?:p|div|tr)>/gi, '\n').replace(/<[^>]+>/g, '').trim();
  }
  const el = document.createElement('template');
  el.innerHTML = String(html);
  for (const node of el.content.querySelectorAll('br')) node.replaceWith('\n');
  for (const node of el.content.querySelectorAll('p,div,tr')) node.append('\n');
  for (const node of el.content.querySelectorAll('td,th')) node.append(' ');
  return el.content.textContent.replace(/\r\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
}

function chineseBlankIndex(token) {
  if (!token) return null;
  const s = String(token).trim();
  if (/^\d+$/.test(s)) return Number(s);
  const circled = '①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳';
  const ci = circled.indexOf(s);
  if (ci >= 0) return ci + 1;
  const map = { '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10 };
  if (map[s]) return map[s];
  if (/^十[一二三四五六七八九]$/.test(s)) return 10 + map[s[1]];
  if (/^[二三四五六七八九]十[一二三四五六七八九]?$/.test(s)) return map[s[0]] * 10 + (map[s[2]] || 0);
  return null;
}

function parseBlankRuleLines(note, parts) {
  const lines = String(note || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  const count = Array.isArray(parts) ? parts.length : 0;
  if (!lines.length || !count) return { aligned: false, byIndex: [] };
  if (lines.length === count) return { aligned: true, byIndex: lines };

  const buckets = Array.from({ length: count }, () => []);
  let matched = 0;
  const prefixRe = /^(?:第\s*([一二三四五六七八九十\d]+)\s*(?:空|要素|点)|[（(]\s*(\d+)\s*[)）]|([①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳])|(\d+)\s*[.．、:：])/;
  for (const line of lines) {
    const m = prefixRe.exec(line);
    const idx = m ? chineseBlankIndex(m[1] || m[2] || m[3] || m[4]) : null;
    if (idx && idx >= 1 && idx <= count) {
      buckets[idx - 1].push(line);
      matched++;
    } else if (matched > 0) {
      const last = buckets.findLastIndex(b => b.length > 0);
      if (last >= 0) buckets[last].push(line);
    }
  }
  if (matched >= count && buckets.every(b => b.length > 0)) {
    return { aligned: true, byIndex: buckets.map(b => b.join('；')) };
  }
  return { aligned: false, byIndex: [] };
}

function extractAcceptedAnswers(ruleText, blankScore = null) {
  const raw = String(ruleText || '').trim();
  if (!raw) return [];
  const stripped = raw
    .replace(/\s*[（(]\s*\d+(?:\.\d+)?\s*分\s*[)）]\s*$/, '')
    .replace(/^(?:第\s*[一二三四五六七八九十\d]+\s*(?:空|要素|点)(?:\s*[（(]\s*\d+(?:\.\d+)?\s*分\s*[)）])?\s*[:：]?|[（(]\s*\d+\s*[)）]\s*[:：]?|[①②③④⑤⑥⑦⑧⑨⑩]\s*[:：]?|\d+\s*[.．、:：]\s*)/, '')
    .trim();
  if (!stripped) return [];

  const negRe = /(?:给\s*0\s*分|得\s*0\s*分|记\s*0\s*分|计\s*0\s*分|0\s*分|不给分|不得分|不计分|扣\s*\d|错答|错写|错别字|写错|漏写|少答|未答|只答.{0,24}(?:给\s*0|不给|不得))/;
  const openOnlyRe = /^(?:意思相近|合理即可|合理给分|言之有理|答出任意|答出两点|答出\d+点)/;
  const clauses = stripped.split(/[；;。]/).map(s => s.trim()).filter(Boolean);
  const found = [];
  const add = val => {
    const clean = String(val || '').trim();
    if (!clean || clean.length > 48) return;
    if (/(?:给\s*\d|得\s*\d|不给分|不得分|扣分|合理即可|意思相近)/.test(clean)) return;
    if (!found.includes(clean)) found.push(clean);
  };

  for (const clause of clauses) {
    if (openOnlyRe.test(clause)) continue;
    const subClauses = clause.split(/[，,]/).map(s => s.trim()).filter(Boolean);
    const positiveSubs = [];
    for (const sub of subClauses) {
      if (negRe.test(sub) || openOnlyRe.test(sub)) continue;
      if (blankScore != null && blankScore > 0) {
        const sm = sub.match(/(?:给|得)\s*(\d+(?:\.\d+)?)\s*分/);
        if (sm && Number(sm[1]) < blankScore) continue;
        if (/^(?:只答|仅答|漏答|答出其中)/.test(sub)) continue;
      }
      positiveSubs.push(sub);
    }
    const positiveText = positiveSubs.join('，');
    if (!positiveText) continue;

    const quoteRe = /[“「『"]([^”」』"]+)[”」』"]/g;
    let qm;
    let quotedInClause = 0;
    while ((qm = quoteRe.exec(positiveText)) !== null) {
      add(qm[1]);
      quotedInClause++;
    }
    if (quotedInClause > 0) continue;

    const simple = positiveText.match(/^(?:答出?|写出?|填|回答)?\s*([^，,；;。]{1,28}?)\s*(?:即可(?:给分|得分)?|(?:均)?(?:给|得)\s*(?:满分|\d+(?:\.\d+)?\s*分))$/);
    if (simple) {
      const expr = simple[1].trim();
      if (expr && !/(?:合理|意思|原因|理由|两点|至少|说明|解释|体现|包含|机制|过程|正确|完整|相关|任意)/.test(expr)) {
        for (const piece of expr.split(/\s*(?:或|\/|／)\s*/)) add(piece);
      }
    }
  }
  return found;
}

// ---------- Per-Blank Reference Answer Splitting & Biological Accepted Variants ----------

function decodeBasicEntities(str) {
  return String(str || '')
    .replace(/&nbsp;/gi, ' ')
    .replace(/&gt;/gi, '>')
    .replace(/&lt;/gi, '<')
    .replace(/&amp;/gi, '&')
    .replace(/&quot;/gi, '"')
    .replace(/&#39;|&apos;/gi, "'");
}

function htmlToPlainLinesForBlanks(html, opts = {}) {
  if (!html) return [];
  const keepImgPlaceholder = Boolean(opts.keepImgPlaceholder);
  if (typeof document !== 'undefined') {
    const el = document.createElement('template');
    el.innerHTML = String(html);
    if (keepImgPlaceholder) {
      for (const img of el.content.querySelectorAll('img')) img.replaceWith(' [见参考答案图] ');
    }
    const lines = [];
    let buf = '';
    const boundary = new Set(['P', 'DIV', 'BR', 'TD', 'TH', 'TR', 'LI']);
    const flush = () => {
      if (buf.trim()) lines.push(buf);
      buf = '';
    };
    const walk = node => {
      if (node.nodeType === 3) { buf += node.data; return; }
      if (node.nodeName === 'IMG') {
        if (keepImgPlaceholder) buf += ' [见参考答案图] ';
        else flush();
        return;
      }
      const isB = boundary.has(node.nodeName);
      if (isB) flush();
      for (const ch of node.childNodes) walk(ch);
      if (isB) flush();
    };
    walk(el.content);
    flush();
    return lines;
  }

  let raw = String(html);
  if (keepImgPlaceholder) {
    raw = raw.replace(/<img\b[^>]*>/gi, ' [见参考答案图] ');
  } else {
    raw = raw.replace(/<img\b[^>]*>/gi, '\n');
  }
  raw = raw
    .replace(/<br\s*\/?>/gi, '\n')
    .replace(/<\/?(?:p|div|tr|td|th|li)\b[^>]*>/gi, '\n')
    .replace(/<[^>]+>/g, '');
  return decodeBasicEntities(raw)
    .split(/\r?\n/)
    .map(s => s.replace(/\u00a0/g, ' '))
    .filter(s => s.trim() !== '');
}

function getQuestionFaceHtmlForBlanks(q) {
  if (!q) return '';
  if (typeof editionFace === 'function' && typeof document !== 'undefined') {
    return editionFace(q);
  }
  if (q.contentHtml) {
    return String(q.contentHtml).replace(/^(<(?:p|div)>\s*)(?:(例\s*\d+)\s*[.．、]?|(\d+)\s*[.．、])[\s\u3000]*/i, (all, start, ex, n) => {
      const label = ex ? ex.replace(/\s/g, '') : n;
      return q.src?.some(ref => String(ref[1]).replace(/\s/g, '') === label) ? start : all;
    });
  }
  const body = q.body && q.body !== q.stem && !String(q.body).startsWith(String(q.stem || ''))
    ? (q.stem + '\n' + q.body)
    : (q.body || q.stem || '');
  return String(body);
}

// Mirrors blankLayout(q) in selection-grading.js 1-to-1 while capturing sub-question,
// wrapped-underline continuation, step numerals, and stem unit/headword context per blank.
function extractStemBlankContexts(q) {
  const faceHtml = getQuestionFaceHtmlForBlanks(q);
  const lines = htmlToPlainLinesForBlanks(faceHtml, { keepImgPlaceholder: false });
  const blanks = [];
  let previous = '';
  let currentSub = '';
  let lastSubNum = 0;

  for (const rawLine of lines) {
    const text = rawLine;
    const leadTrimLen = text.length - text.replace(/^[\s\u3000\u00a0]+/, '').length;
    const writingLine = /^\s*[_＿]{2,}(?:\s*[（(]\s*答出[^）)]*[）)])?[。．.\s]*$/.test(text);
    const continuation = writingLine && (
      !previous ||
      /[_＿][。．.\s]*$/.test(previous) ||
      (/^\s*＿{8,}/.test(text) && /[。！？]$/.test(previous))
    );

    const subMarkers = [];
    const subRe = /(?:^|[。；！？:\s\u3000])(?:[ⅠⅡⅢⅣⅤ]\s*[.．、]?\s*)?[（(](\d{1,2})[）)]/g;
    for (const m of text.matchAll(subRe)) {
      const num = Number(m[1]);
      if (num < 1 || num > 20) continue;
      const parenOffset = m.index + m[0].search(/[（(]/);
      const atStart = parenOffset <= leadTrimLen + 5;
      const ok = (lastSubNum === 0 && num <= 2) ||
        num === lastSubNum + 1 ||
        (atStart && num > lastSubNum && num <= lastSubNum + 3);
      if (ok) {
        subMarkers.push({ offset: parenOffset, num });
        lastSubNum = num;
      }
    }
    let mPos = 0;
    const advanceSub = limit => {
      while (mPos < subMarkers.length && subMarkers[mPos].offset <= limit) {
        currentSub = String(subMarkers[mPos].num);
        mPos++;
      }
    };

    if (!continuation) {
      for (const match of text.matchAll(/([_＿]{2,})(?:\s*[（(]\s*(\d+(?:\.\d+)?)\s*分\s*[）)])?/g)) {
        const start = match.index;
        const fullEnd = match.index + match[0].length;
        advanceSub(start);

        const isWrappedTail = (start === leadTrimLen) && /[_＿]\s*$/.test(previous);
        const beforeSlice = text.slice(Math.max(0, start - 36), start);
        const beforeClause = beforeSlice.split(/[。；！？\n]/).pop() || '';
        const circledHits = [...beforeClause.matchAll(/([①②③④⑤⑥⑦⑧⑨⑩])/g)];
        const stepMarker = circledHits.length ? circledHits[circledHits.length - 1][1] : '';
        const afterSlice = text.slice(fullEnd, Math.min(text.length, fullEnd + 36));
        const asksSerialNo = /(?:序号|标号|编号)/.test(beforeClause) || /^\s*[（(][^）)]*(?:序号|标号|编号)/.test(afterSlice);

        const askCountM = afterSlice.match(/^\s*[（(]\s*(?:答出|写出|任写|任答|列举)\s*([两二三四\d]+)\s*(?:种|点|项|类|个|条)?/);
        const askCount = askCountM ? (askCountM[1] === '两' ? 2 : (chineseBlankIndex(askCountM[1]) || 0)) : 0;

        const unitM = afterSlice.match(/^\s*(℃|°C|%|‰|mL|ml|L|mg|μg|ug|kg|g|cm|mm|μm|um|nm|km|mol·L(?:⁻|-)?¹|mol\/L|kPa|MPa|min|ms|h|s|个|层|种|条|对|次|倍|分子)(?![a-zA-Z])/);
        const stemUnit = unitM ? unitM[1] : '';
        const headM = !stemUnit ? afterSlice.match(/^\s*(法|细胞|生物|模型|反应|结构|层次|染液|试剂)(?=[，,。；;：:）)\s]|$)/) : null;
        const stemHeadword = headM ? headM[1] : '';

        blanks.push({
          index: blanks.length,
          subKey: currentSub || '',
          isWrappedTail,
          stepMarker,
          asksSerialNo,
          askCount,
          beforeText: beforeClause.slice(-12),
          afterText: afterSlice.slice(0, 24),
          stemUnit,
          stemHeadword
        });
      }
    }
    advanceSub(text.length + 1);
    previous = text.trim();
  }

  if (blanks.some(b => b.subKey === '')) {
    const hasTwo = blanks.some(b => b.subKey === '2');
    const hasOne = blanks.some(b => b.subKey === '1');
    const fallbackKey = (hasTwo && !hasOne) ? '1' : '0';
    for (const b of blanks) {
      if (b.subKey === '') b.subKey = fallbackKey;
    }
  }
  return blanks;
}

function buildBracketDepthMap(str) {
  const depths = new Int16Array(str.length + 1);
  let d = 0;
  for (let i = 0; i < str.length; i++) {
    depths[i] = d;
    const ch = str[i];
    if (ch === '(' || ch === '（' || ch === '[' || ch === '［') d++;
    else if ((ch === ')' || ch === '）' || ch === ']' || ch === '］') && d > 0) d--;
  }
  depths[str.length] = d;
  return depths;
}

function collapseNewlinesInsideBrackets(str) {
  if (!str) return '';
  let out = '';
  let d = 0;
  for (let i = 0; i < str.length; i++) {
    const ch = str[i];
    if (ch === '(' || ch === '（' || ch === '[' || ch === '［') { d++; out += ch; continue; }
    if (ch === ')' || ch === '）' || ch === ']' || ch === '］') { if (d > 0) d--; out += ch; continue; }
    if (d > 0 && (ch === '\r' || ch === '\n' || ch === '\u3000')) {
      continue;
    }
    if (d > 0 && ch === ' ' && (out.endsWith('(') || out.endsWith('（') || out.endsWith('或'))) {
      continue;
    }
    out += ch;
  }
  return out;
}

function splitOutsideBrackets(text, sepRe) {
  const clean = collapseNewlinesInsideBrackets(String(text || '')).trim();
  if (!clean) return [];
  const depths = buildBracketDepthMap(clean);
  const flags = sepRe.flags.includes('g') ? sepRe.flags : sepRe.flags + 'g';
  const re = new RegExp(sepRe.source, flags);
  const parts = [];
  let last = 0;
  for (const m of clean.matchAll(re)) {
    const idx = m.index;
    if (depths[idx] !== 0) {
      if (m[0].length === 0) re.lastIndex = idx + 1;
      continue;
    }
    const seg = clean.slice(last, idx).trim();
    if (seg) parts.push(seg);
    last = idx + m[0].length;
    if (m[0].length === 0) re.lastIndex = idx + 1;
  }
  const tail = clean.slice(last).trim();
  if (tail) parts.push(tail);
  return parts;
}

function splitBucketIntoCount(bucketText, needed) {
  const clean = collapseNewlinesInsideBrackets(String(bucketText || '')).trim();
  if (!clean || needed <= 0) return [];
  if (needed === 1) {
    return [clean.replace(/\s*\n\s*/g, ' ').replace(/[\u3000]{2,}/g, '　').trim()];
  }

  const splitters = [
    /[\u3000]+|\s{2,}|\r?\n+/,
    /[\u3000]+|\s{2,}|\r?\n+|[；;]+/,
    /[\u3000]+|\s{2,}|\r?\n+|[；;]+|(?=(?:^|[\s\u3000])[①②③④⑤⑥⑦⑧⑨⑩]|(?:^|[\s\u3000])[ⅠⅡⅢⅣⅤ]\s*[.．、])/,
    /[\u3000]+|\s{2,}|\r?\n+|[；;、]+/,
    /\s+/
  ];

  const candidates = [];
  for (const sp of splitters) {
    const cand = splitOutsideBrackets(clean, sp);
    candidates.push(cand);
    if (cand.length === needed) return cand;
  }

  // Pick closest candidate, preferring >= needed so we can merge trailing clauses cleanly.
  const over = candidates.filter(c => c.length > needed).sort((a, b) => a.length - b.length)[0];
  if (over) {
    return [
      ...over.slice(0, needed - 1),
      over.slice(needed - 1).join('；')
    ];
  }
  const bestUnder = candidates.sort((a, b) => b.length - a.length)[0] || [clean];
  return bestUnder;
}

function stripStepPrefixForBlank(rawPiece, ctx) {
  let s = String(rawPiece || '').replace(/^[；;、，,\s\u3000]+/, '').trim();
  if (!s) return '';
  // Always strip Roman section prefixes with punctuation (e.g., "Ⅰ.BD", "Ⅱ.②适量...", "Ⅲ.若...")
  s = s.replace(/^[ⅠⅡⅢⅣⅤ]\s*[.．、:：]\s*/, '').trim();
  // Strip circled step marker ①..⑩ when the stem itself used a step marker (and isn't asking for a structure serial number)
  const cm = s.match(/^([①②③④⑤⑥⑦⑧⑨⑩])\s*[.．、:：]?\s*(.+)$/);
  if (cm && !ctx?.asksSerialNo) {
    if (ctx?.stepMarker === cm[1] || ctx?.stepMarker || cm[2].length >= 4) {
      // Preserve diagram labels like "②高尔基体" when stem had no stepMarker and tail is a short organelle name
      const looksLikeDiagramLabel = !ctx?.stepMarker && /^[高内线叶核中液溶拟细染色基质膜体]{2,6}$/.test(cm[2]);
      if (!looksLikeDiagramLabel) {
        s = cm[2].trim();
      }
    }
  }
  return s;
}

function buildPermutations(arr, maxCount = 6) {
  if (arr.length <= 1) return [arr.slice()];
  if (arr.length === 2) return [[arr[0], arr[1]], [arr[1], arr[0]]];
  const out = [];
  const used = new Array(arr.length).fill(false);
  const cur = [];
  const dfs = () => {
    if (out.length >= maxCount) return;
    if (cur.length === arr.length) { out.push(cur.slice()); return; }
    for (let i = 0; i < arr.length; i++) {
      if (used[i]) continue;
      used[i] = true;
      cur.push(arr[i]);
      dfs();
      cur.pop();
      used[i] = false;
    }
  };
  dfs();
  return out;
}

function splitReferenceAnswerByBlanks(q, answerHtmlOrText, targetCount) {
  if (!targetCount || targetCount <= 0) return [];
  const ansLines = htmlToPlainLinesForBlanks(answerHtmlOrText, { keepImgPlaceholder: true });
  let fullText = ansLines.join('\n').trim();
  fullText = fullText.replace(/^[\s\u3000]*(?:【?(?:参考|标准)?答案】?|(?:参考|标准)?答案\s*[:：]?)[\s\u3000]*/i, '').trim();
  if (!fullText) return Array.from({ length: targetCount }, () => ({ primary: '', extras: [], ctx: null }));

  const stemBlanks = extractStemBlankContexts(q);
  const alignedWithStem = stemBlanks.length === targetCount;

  if (alignedWithStem) {
    const subOrder = [];
    const subGroups = new Map();
    for (const b of stemBlanks) {
      if (!subGroups.has(b.subKey)) {
        subGroups.set(b.subKey, []);
        subOrder.push(b.subKey);
      }
      subGroups.get(b.subKey).push(b);
    }

    const numberedKeys = subOrder.filter(k => Number(k) >= 1);
    const bucketMap = new Map();
    if (numberedKeys.length) {
      const expected = new Set(numberedKeys.map(Number));
      const collapsedFull = collapseNewlinesInsideBrackets(fullText);
      const depths = buildBracketDepthMap(collapsedFull);
      const hits = [];
      let lastHitNum = 0;
      const subRe = /(?:^|[。；！？\s\u3000\n])(?:[ⅠⅡⅢⅣⅤ]\s*[.．、]?\s*)?[（(](\d{1,2})[）)]/g;
      for (const m of collapsedFull.matchAll(subRe)) {
        const parenPos = m.index + m[0].search(/[（(]/);
        if (depths[parenPos] !== 0) continue;
        const num = Number(m[1]);
        if (!expected.has(num) || num <= lastHitNum) continue;
        const sepLead = m[0].match(/^[。；！？\s\u3000\n]*/)?.[0].length || 0;
        hits.push({
          key: String(num),
          markerStart: m.index + sepLead,
          bodyStart: m.index + m[0].length
        });
        lastHitNum = num;
      }
      for (let hi = 0; hi < hits.length; hi++) {
        const end = hi + 1 < hits.length ? hits[hi + 1].markerStart : collapsedFull.length;
        const seg = collapsedFull.slice(hits[hi].bodyStart, end).trim();
        if (seg) bucketMap.set(hits[hi].key, seg);
      }
      if (hits.length && hits[0].markerStart > 0 && subGroups.has('1') && !bucketMap.has('1')) {
        const lead = collapsedFull.slice(0, hits[0].markerStart).trim();
        if (lead) bucketMap.set('1', lead);
      }
    }
    if (!bucketMap.size && subOrder.length === 1) {
      bucketMap.set(subOrder[0], collapseNewlinesInsideBrackets(fullText));
    }

    const slots = Array.from({ length: targetCount }, (_, i) => ({ primary: '', extras: [], ctx: stemBlanks[i] }));
    let allMatched = true;

    for (const key of subOrder) {
      const gBlanks = subGroups.get(key) || [];
      const bText = bucketMap.get(key) || '';
      if (!bText) { allMatched = false; break; }

      // Group paragraph-wrapped underline continuations onto their head blank.
      const logicalGroups = [];
      for (const b of gBlanks) {
        if (b.isWrappedTail && logicalGroups.length > 0) {
          logicalGroups[logicalGroups.length - 1].push(b);
        } else {
          logicalGroups.push([b]);
        }
      }

      // First check if raw non-merged count matches directly; otherwise split by logicalGroups.length.
      const directPieces = splitOutsideBrackets(bText, /[\u3000]+|\s{2,}|\r?\n+|[；;]+/);
      if (directPieces.length === gBlanks.length) {
        for (let i = 0; i < gBlanks.length; i++) {
          const b = gBlanks[i];
          slots[b.index] = {
            primary: stripStepPrefixForBlank(directPieces[i], b),
            extras: [],
            ctx: b
          };
        }
        continue;
      }

      const logicalPieces = splitBucketIntoCount(bText, logicalGroups.length);
      if (logicalPieces.length !== logicalGroups.length) {
        allMatched = false;
        break;
      }

      for (let li = 0; li < logicalGroups.length; li++) {
        const spanBlanks = logicalGroups[li];
        const headCtx = spanBlanks[0];
        const tailCtx = spanBlanks[spanBlanks.length - 1];
        const mergedCtx = {
          ...headCtx,
          afterText: tailCtx.afterText || headCtx.afterText,
          askCount: tailCtx.askCount || headCtx.askCount,
          stemUnit: tailCtx.stemUnit || headCtx.stemUnit,
          stemHeadword: tailCtx.stemHeadword || headCtx.stemHeadword
        };
        const cleanPiece = stripStepPrefixForBlank(logicalPieces[li], mergedCtx);

        if (spanBlanks.length === 1) {
          slots[headCtx.index] = { primary: cleanPiece, extras: [], ctx: mergedCtx };
        } else {
          // Wrapped underline spanning spanBlanks.length physical lines (e.g. Q14 Blank 1 & Blank 2).
          const coordItems = splitOutsideBrackets(cleanPiece.replace(/[（(]\s*(?:写出|答出|任答)[^）)]*[）)]\s*$/, ''), /\s*(?:、|和|与|及)\s*/);
          if (coordItems.length === spanBlanks.length && coordItems.every(it => it.length >= 1 && it.length <= 16)) {
            for (let si = 0; si < spanBlanks.length; si++) {
              const otherItems = coordItems.filter((_, idx) => idx !== si);
              slots[spanBlanks[si].index] = {
                primary: coordItems[si],
                extras: [cleanPiece, ...otherItems],
                ctx: { ...spanBlanks[si], ...mergedCtx, index: spanBlanks[si].index }
              };
            }
          } else if (coordItems.length > spanBlanks.length && mergedCtx.askCount === spanBlanks.length && coordItems.every(it => it.length <= 16)) {
            for (let si = 0; si < spanBlanks.length; si++) {
              slots[spanBlanks[si].index] = {
                primary: coordItems[si],
                extras: [cleanPiece, ...coordItems],
                ctx: { ...spanBlanks[si], ...mergedCtx, index: spanBlanks[si].index }
              };
            }
          } else {
            for (let si = 0; si < spanBlanks.length; si++) {
              slots[spanBlanks[si].index] = {
                primary: cleanPiece,
                extras: [],
                ctx: { ...spanBlanks[si], ...mergedCtx, index: spanBlanks[si].index }
              };
            }
          }
        }
      }
    }

    if (allMatched && slots.every(s => s.primary)) {
      return slots;
    }
  }

  // Fallback for questions whose parts count was manually customized in #grading or had unnumbered answers.
  const strippedMarkers = collapseNewlinesInsideBrackets(fullText)
    .replace(/(?:^|(?<=[\s\u3000\n。；;]))(?:[ⅠⅡⅢⅣⅤ]\s*[.．、]?\s*)?[（(]\d{1,2}[）)]\s*/g, '　')
    .trim();
  const flatPieces = splitBucketIntoCount(strippedMarkers, targetCount);
  return Array.from({ length: targetCount }, (_, i) => {
    const ctx = stemBlanks[i] || null;
    const raw = flatPieces[i] || flatPieces[flatPieces.length - 1] || '';
    return {
      primary: stripStepPrefixForBlank(raw, ctx),
      extras: [],
      ctx
    };
  });
}

// Strict biological proper terms (organelles, structures, reagents, processes) take their canonical form ONLY
// (never invent wrong-character typos such as "高尔基休" or "溶菌体").
// Curated map covers legitimate textbook synonymous expressions in high-school biology.
const BIOLOGY_SYNONYM_MAP = new Map([
  ['一定的流动性', ['流动性', '具有一定的流动性', '具有流动性']],
  ['流动性', ['一定的流动性', '具有流动性', '具有一定的流动性']],
  ['选择透过性', ['具有选择透过性']],
  ['多种水解酶', ['水解酶', '大量水解酶', '大量的水解酶']],
  ['H＋的梯度势能', ['H+的梯度势能', 'H⁺的梯度势能', 'H+梯度势能', 'H＋梯度势能', 'H+电化学梯度', 'H＋电化学梯度', 'H⁺电化学梯度', 'H+的电化学梯度', 'H+电化学势能', 'H+浓度梯度', 'H+浓度差', '细胞膜内外两侧的H+浓度差']],
  ['H+的梯度势能', ['H＋的梯度势能', 'H⁺的梯度势能', 'H+梯度势能', 'H+电化学梯度', 'H＋电化学梯度', 'H⁺电化学梯度', 'H+的电化学梯度', 'H+电化学势能', 'H+浓度梯度', 'H+浓度差', '细胞膜内外两侧的H+浓度差']],
  ['是否需要网格蛋白介导', ['是否需要网格蛋白参与', '是否需要网格蛋白', '有无网格蛋白介导', '有无网格蛋白参与', '是否依赖网格蛋白']],
  ['细胞再次利用或者排出细胞', ['细胞再次利用或排出细胞', '排出细胞或者细胞再次利用', '排出细胞或细胞再次利用', '细胞再次利用、排出细胞', '排出细胞、细胞再次利用', '被细胞再次利用或排出细胞外', '排出细胞外或被细胞再次利用', '排出细胞外或被细胞重新利用', '被细胞重新利用或排出细胞外']],
  ['排出细胞外或被细胞重新利用', ['被细胞重新利用或排出细胞外', '细胞再次利用或者排出细胞', '细胞再次利用或排出细胞', '排出细胞或细胞再次利用', '被细胞再次利用或排出细胞外']],
  ['细胞膜上转运蛋白的种类和数量', ['细胞膜上转运蛋白的种类和数目', '膜上转运蛋白的种类和数量', '膜上转运蛋白的种类和数目', '转运蛋白的种类和数量', '转运蛋白的种类和数目', '细胞膜上载体蛋白和通道蛋白的种类和数量']],
  ['膜上转运蛋白的种类和数量', ['膜上转运蛋白的种类和数目', '细胞膜上转运蛋白的种类和数量', '转运蛋白的种类和数量']],
  ['土壤盐分过多，土壤溶液浓度大，甚至大于植物根部细胞细胞液浓度', ['土壤溶液浓度大于植物根部细胞细胞液浓度', '土壤溶液浓度高于植物根部细胞细胞液浓度', '土壤溶液浓度大于根细胞细胞液浓度', '土壤溶液浓度高于植物根毛细胞液浓度', '土壤溶液浓度大于细胞液浓度', '土壤溶液浓度高于细胞液浓度']],
  ['土壤溶液浓度高于植物根毛细胞液浓度', ['土壤溶液浓度大于植物根毛细胞液浓度', '土壤溶液浓度大于植物根部细胞细胞液浓度', '土壤溶液浓度高于细胞液浓度', '土壤溶液浓度大于细胞液浓度', '土壤盐分过多，土壤溶液浓度大，甚至大于植物根部细胞细胞液浓度']],
  ['外界溶液浓度高于细胞液浓度', ['外界溶液浓度大于细胞液浓度']],
  ['外界溶液浓度大于细胞液浓度', ['外界溶液浓度高于细胞液浓度']],
  ['没有以核膜为界限的细胞核', ['无以核膜为界限的细胞核', '没有核膜包被的细胞核', '无核膜包被的细胞核', '没有成形的细胞核', '无成形的细胞核']],
  ['原核细胞没有以核膜为界限的细胞核', ['没有以核膜为界限的细胞核', '无以核膜为界限的细胞核', '无核膜包被的细胞核', '没有成形的细胞核']],
  ['以核膜为界限的细胞核', ['核膜包被的细胞核', '成形的细胞核']],
  ['同位素标记法', ['同位素示踪法']],
  ['放射性同位素标记法', ['同位素标记法', '同位素示踪法']],
  ['控制物质进出细胞', ['控制物质进出']],
  ['双缩脲试剂', ['双缩脲']]
]);

const SMALL_INT_CN_MAP = {
  '1': '一', '2': '二', '3': '三', '4': '四', '5': '五',
  '6': '六', '7': '七', '8': '八', '9': '九', '10': '十'
};

const SUB_SUP_TO_ASCII_MAP = {
  '₀': '0', '₁': '1', '₂': '2', '₃': '3', '₄': '4', '₅': '5', '₆': '6', '₇': '7', '₈': '8', '₉': '9',
  '⁰': '0', '¹': '1', '²': '2', '³': '3', '⁴': '4', '⁵': '5', '⁶': '6', '⁷': '7', '⁸': '8', '⁹': '9'
};

function normalizeChemAndMathSymbols(str) {
  let out = String(str || '');
  out = out.replace(/[₀₁₂₃₄₅₆₇₈₉⁰¹²³⁴⁵⁶⁷⁸⁹]/g, ch => SUB_SUP_TO_ASCII_MAP[ch] || ch);
  out = out
    .replace(/[＋⁺]/g, '+')
    .replace(/[－⁻—–]/g, '-')
    .replace(/＝/g, '=')
    .replace(/＞/g, '>')
    .replace(/＜/g, '<')
    .replace(/～/g, '~');
  return out;
}

function cleanAnswerMetaNotes(raw) {
  let s = String(raw || '').trim();
  if (!s) return { cleaned: '', orderFree: false };
  const orderFree = /顺序(?:可颠倒|不限|无关)/.test(s);
  // Strip trailing grading instructions like "(写出两种即可)" "(合理即可)" "(顺序不限)"
  s = s.replace(/\s*[（(]\s*(?:写出|答出|任写|任答|列举|只写|至少|合理即可|其他合理|意思相近|顺序可颠倒|顺序不限)[^）)]*[）)]\s*$/g, '').trim();
  s = s.replace(/[。．.\s]+$/, '').trim();
  return { cleaned: s, orderFree };
}

function unpackInlineAnswerAlternatives(raw) {
  const { cleaned } = cleanAnswerMetaNotes(raw);
  if (!cleaned) return [];
  if (cleaned === '[见参考答案图]') return ['见参考答案图'];

  const results = [];
  const push = v => {
    const t = String(v || '').replace(/^[；;、\s\u3000]+|[。．.\s\u3000]+$/g, '').trim();
    if (t && !results.includes(t)) results.push(t);
  };

  // 1. Trailing parenthetical "(或 X)" / "（或 X）" -> e.g. "遗传信息(或DNA)", "低温(或－273 ℃)", "酶(或ATP水解酶)"
  const trailOr = cleaned.match(/^(.+?)\s*[（(]\s*或\s*([^）)]+?)\s*[）)]$/);
  if (trailOr) {
    const base = trailOr[1].trim();
    const alts = trailOr[2].split(/\s*(?:或|、|\/|／)\s*/).map(x => x.trim()).filter(Boolean);
    for (const u of unpackInlineAnswerAlternatives(base)) push(u);
    for (const a of alts) {
      for (const u of unpackInlineAnswerAlternatives(a)) push(u);
    }
    return results;
  }

  // 2. Trailing synonym/elaboration parenthetical without "或":
  //    - "几丁质（壳多糖）" -> "几丁质", "壳多糖"
  //    - "内质网(膜)" -> "内质网", "内质网膜"
  //    - "糖原（肝糖原、肌糖原）" -> "糖原", "肝糖原、肌糖原"
  const trailParen = cleaned.match(/^([^（(]+?)\s*[（(]([^）)]+)[）)]$/);
  if (trailParen) {
    const head = trailParen[1].trim();
    const inner = trailParen[2].trim();
    if (inner.length <= 2) {
      // Short suffix modifier like "内质网(膜)" or "内质网(甲)"
      if (/^[甲乙丙丁戊ⅠⅡⅢⅣⅤA-Za-z0-9①-⑩]$/.test(inner)) {
        push(head);
        push(cleaned);
      } else {
        push(head);
        push(head + inner);
        push(cleaned);
      }
      return results;
    }
    if (!/[，,。；;：:]/.test(inner) && inner.length <= 16) {
      push(head);
      push(inner);
      push(cleaned);
      return results;
    }
  }

  // 3. Leading optional modifier "(一定的)流动性" / "(中央)大液泡"
  const leadOpt = cleaned.match(/^[（(]([^）)]{1,6})[）)](.+)$/);
  if (leadOpt) {
    push(leadOpt[1].trim() + leadOpt[2].trim());
    push(leadOpt[2].trim());
    return results;
  }

  // 4. Mid-word optional modifier "脱氧(核糖)核苷酸", "具有(中央)大液泡", "氨基(—NH2)和羧基(—COOH)"
  if (/[（(][^）)]{1,10}[）)]/.test(cleaned)) {
    const withoutParens = cleaned.replace(/\s*[（(][^）)]{1,12}[）)]\s*/g, '').trim();
    const unwrapped = cleaned.replace(/[（(]([^）)]{1,6})[）)]/g, '$1').trim();
    const parenMatches = [...cleaned.matchAll(/[（(]([^）)]{1,12})[）)]/g)].map(m => m[1].trim());
    // Chemical formula pair inside parens: "氨基(—NH2)和羧基(—COOH)"
    if (parenMatches.length === 2 && parenMatches.every(p => /^[-—－]?[A-Za-z0-9₂₃₄⁺⁻+]+$/.test(p)) && /和|与|、/.test(withoutParens)) {
      push(withoutParens);
      push(parenMatches[0] + '和' + parenMatches[1]);
      push(cleaned);
      return results;
    }
    if (!/[—－-]/.test(parenMatches.join('')) && unwrapped !== cleaned && unwrapped.length <= 24) {
      push(unwrapped);
    }
    if (withoutParens && withoutParens !== cleaned) push(withoutParens);
    push(cleaned);
    return results;
  }

  push(cleaned);
  return results;
}

function addSingleSeedVariants(seed, ctx, push) {
  const s = String(seed || '').trim();
  if (!s) return;
  push(s);

  // 1. Curated biology textbook synonymous expressions (strict proper nouns stay canonical)
  const directSyns = BIOLOGY_SYNONYM_MAP.get(s);
  if (directSyns) {
    for (const syn of directSyns) push(syn);
  }
  const normSym = normalizeChemAndMathSymbols(s);
  if (normSym !== s) {
    push(normSym);
    const normSyns = BIOLOGY_SYNONYM_MAP.get(normSym);
    if (normSyns) {
      for (const syn of normSyns) push(syn);
    }
  }

  // Fullwidth plus / superscript plus variants (e.g. H+ / H＋ / H⁺, Ca2+ / Ca²⁺)
  if (/[+＋⁺]/.test(s)) {
    push(s.replace(/[+＋⁺]/g, '+'));
    push(s.replace(/[+＋⁺]/g, '＋'));
    push(s.replace(/[+＋⁺]/g, '⁺'));
  }
  // Dash / bond variants (e.g. —CH2SH / -CH2SH / －CH2SH, —NH2和—COOH -> -NH2和-COOH)
  if (/[—－–-][A-Za-z0-9]/.test(s) && !/^\d/.test(s)) {
    push(s.replace(/[—－–-](?=[A-Za-z0-9])/g, '-'));
    push(s.replace(/[—－–-](?=[A-Za-z0-9])/g, '—'));
    push(s.replace(/[—－–-](?=[A-Za-z0-9])/g, '－'));
  }

  // 2. Diagram label + organelle name spacing: "②高尔基体" -> "② 高尔基体", "②、高尔基体"
  const diagM = s.match(/^([①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮])([\u4e00-\u9fa5]{2,10})$/);
  if (diagM) {
    push(diagM[1] + ' ' + diagM[2]);
    push(diagM[1] + '、' + diagM[2]);
  }

  // Arrow chain variants: "⑥→⑨→④→①"
  if (/→/.test(s) && s.length <= 24) {
    push(s.replace(/\s*→\s*/g, '->'));
    push(s.replace(/\s*→\s*/g, '-'));
    if (/^[①-⑳→\s]+$/.test(s)) {
      push(s.replace(/\s*→\s*/g, ''));
      push(s.replace(/\s*→\s*/g, '、'));
    }
  }

  // Circled sequence without separators: "⑧⑨⑫" / "⑥⑨④①" -> "⑧、⑨、⑫" (+ "→" when sequence)
  if (/^[①-⑳]{2,6}$/.test(s)) {
    const chars = Array.from(s);
    if (ctx?.beforeText && /(?:顺序|依次|途径|路线)/.test(ctx.beforeText)) {
      push(chars.join('→'));
      push(chars.join('->'));
    }
    push(chars.join('、'));
    push(chars.join('，'));
    push(chars.join(','));
  }

  // Multi-choice letters inside a blank: "ABD" -> "A、B、D", "A,B,D"
  if (/^[A-H]{2,6}$/.test(s)) {
    const letters = s.split('');
    push(letters.join('、'));
    push(letters.join(','));
    push(letters.join('，'));
    push(letters.join(' '));
  }

  // 3. Numeric, range, and unit variants ("数值类带单位变体")
  const stemUnit = ctx?.stemUnit || '';
  // 3a. Pure integer/decimal: e.g. "520", "4", "2", "11.0", "-273"
  if (/^[-－]?\d+(?:\.\d+)?$/.test(s)) {
    const asciiNum = s.replace(/^－/, '-');
    push(asciiNum);
    if (stemUnit) {
      push(asciiNum + stemUnit);
      if (/^[a-zA-Z%℃°]/.test(stemUnit)) {
        push(asciiNum + ' ' + stemUnit);
        if (stemUnit === '℃' || stemUnit === '°C') {
          push(asciiNum + '℃');
          push(asciiNum + '°C');
        }
      }
      if (/^(?:个|层|种|条|对|次|倍|分子)$/.test(stemUnit) && SMALL_INT_CN_MAP[asciiNum]) {
        const cn = asciiNum === '2' ? '两' : SMALL_INT_CN_MAP[asciiNum];
        push(cn);
        push(cn + stemUnit);
        if (asciiNum === '2') {
          push('二');
          push('二' + stemUnit);
        }
      }
    }
  }

  // 3b. Numeric range (with or without embedded unit/suffix): e.g. "50～65", "50～65 ℃水浴"
  const rangeM = s.match(/^(\d+(?:\.\d+)?)\s*[～~—－-]\s*(\d+(?:\.\d+)?)\s*(.*)$/);
  if (rangeM) {
    const [, a, b, rest] = rangeM;
    const cleanRest = rest.trim();
    for (const sep of ['～', '~', '-', '—', '至']) {
      if (cleanRest) {
        push(`${a}${sep}${b}${cleanRest}`);
        push(`${a}${sep}${b} ${cleanRest}`);
        const noSpaceRest = cleanRest.replace(/\s+/g, '');
        if (noSpaceRest !== cleanRest) push(`${a}${sep}${b}${noSpaceRest}`);
      } else {
        push(`${a}${sep}${b}`);
        if (stemUnit) {
          push(`${a}${sep}${b}${stemUnit}`);
          if (stemUnit === '℃' || stemUnit === '°C') push(`${a}${sep}${b}°C`);
        }
      }
    }
  }

  // 3c. Embedded quantity + unit inside answer (e.g. "2 mL", "－273 ℃", "新鲜的普通玉米籽粒组织样液2 mL")
  const qtyUnitM = s.match(/^(.*?)([-－]?\d+(?:\.\d+)?)\s*(mL|ml|L|mg|μg|ug|kg|g|cm|mm|μm|um|nm|℃|°C|%|h|min|s|mol\/L|mol·L(?:⁻|-)?¹)$/);
  if (qtyUnitM) {
    const [, pre, numRaw, unit] = qtyUnitM;
    const num = numRaw.replace(/^－/, '-');
    push(`${pre}${num}${unit}`);
    push(`${pre}${num} ${unit}`);
    if (unit === 'mL' || unit === 'ml') {
      push(`${pre}${num}mL`);
      push(`${pre}${num} mL`);
      push(`${pre}${num}ml`);
      push(`${pre}${num}毫升`);
    } else if (unit === '℃' || unit === '°C') {
      push(`${pre}${num}℃`);
      push(`${pre}${num} ℃`);
      push(`${pre}${num}°C`);
      if (numRaw.startsWith('－') || numRaw.startsWith('-')) {
        push(`${pre}－${num.slice(1)}℃`);
        push(`${pre}－${num.slice(1)} ℃`);
      }
    }
  }

  // 4. Stem boundary echo variants (e.g., "____法" -> "放射性同位素标记法", "____细胞" -> "低等植物细胞")
  const headword = ctx?.stemHeadword || '';
  if (headword && s.length <= 16 && !/[，,。；;：:]/.test(s) && !s.endsWith(headword)) {
    push(s + headword);
  }
  if (ctx?.beforeText && /具有$/.test(ctx.beforeText) && /^(?:一定的)?流动性$|^选择透过性$/.test(s)) {
    push('具有' + s);
  }

  // 5. Coordinate phrases & connector/order variants
  // 5a. Chemical element lists: "C、H、O", "N、P", "K、Ca、P、Mg"
  if (/^[A-Z][a-z]?(?:[、,，\s]+[A-Z][a-z]?){1,5}$/.test(s)) {
    const els = s.split(/[、,，\s]+/).filter(Boolean);
    push(els.join('、'));
    push(els.join('，'));
    push(els.join(','));
    push(els.join(' '));
  }

  // 5b. Modifier-attached pair: "P的X和Y" (e.g. "细胞膜上转运蛋白的种类和数量") -> swap only X and Y!
  const modPairM = s.match(/^(.+的)([\u4e00-\u9fa5]{2,6})(和|与|、)([\u4e00-\u9fa5]{2,6})$/);
  if (modPairM) {
    const [, prefix, x, , y] = modPairM;
    push(`${prefix}${x}和${y}`);
    push(`${prefix}${y}和${x}`);
    push(`${prefix}${x}、${y}`);
    if (y === '数量') push(`${prefix}${x}和数目`);
    if (x === '数量') push(`${prefix}数目和${y}`);
  } else if (!/[，。；：>＞<＜=＝→]/.test(s) && s.length <= 24) {
    // 5c. Dual-fate "A或者B" / "A或B" (keep BOTH halves — never award full marks to a half-answer!)
    const eitherParts = s.split(/\s*(?:或者|或)\s*/).filter(Boolean);
    if (eitherParts.length === 2 && eitherParts.every(p => p.length >= 2 && p.length <= 14)) {
      const [a, b] = eitherParts;
      push(`${a}或${b}`);
      push(`${a}或者${b}`);
      push(`${b}或${a}`);
      push(`${b}或者${a}`);
      push(`${a}、${b}`);
      push(`${b}、${a}`);
    } else {
      // 5d. Pure coordinate list "主动运输、协助扩散", "细胞膜和液泡膜", "甲和乙", "内质网、高尔基体和线粒体"
      const coordParts = s.split(/\s*(?:、|和|与)\s*/).filter(Boolean);
      if (coordParts.length >= 2 && coordParts.length <= 3 &&
          coordParts.every(p => p.length >= 1 && p.length <= 10 && !p.includes('的'))) {
        const perms = buildPermutations(coordParts, 6);
        for (const pm of perms) {
          push(pm.join('、'));
          if (pm.length === 2) {
            push(pm.join('和'));
            push(pm.join('，'));
            push(pm.join('与'));
          } else if (pm.length === 3) {
            push(`${pm[0]}、${pm[1]}和${pm[2]}`);
          }
        }
      }
    }
  }

  // 6. Osmotic / concentration comparison phrasing ("高于" <-> "大于", "低于" <-> "小于")
  if (/(?:浓度|渗透压)/.test(s) && /(?:高于|大于|低于|小于)/.test(s) && s.length <= 32) {
    if (s.includes('高于')) push(s.replace(/高于/g, '大于'));
    if (s.includes('大于')) push(s.replace(/大于/g, '高于'));
    if (s.includes('低于')) push(s.replace(/低于/g, '小于'));
    if (s.includes('小于')) push(s.replace(/小于/g, '低于'));
  }
}

function buildBlankAcceptedList(slotInfo, fromRule = [], maxVariants = 16) {
  const out = [];
  const push = val => {
    const clean = String(val || '').replace(/^[\s\u3000]+|[\s\u3000]+$/g, '');
    if (!clean || out.length >= maxVariants) return;
    if (!out.includes(clean)) out.push(clean);
  };

  const ctx = slotInfo?.ctx || null;
  const primarySeeds = slotInfo?.primary ? unpackInlineAnswerAlternatives(slotInfo.primary) : [];
  const extraSeeds = [];
  for (const ex of slotInfo?.extras || []) {
    for (const u of unpackInlineAnswerAlternatives(ex)) extraSeeds.push(u);
  }
  const ruleSeeds = [];
  for (const rl of fromRule || []) {
    for (const u of unpackInlineAnswerAlternatives(rl)) ruleSeeds.push(u);
  }

  // Ensure accepted[0] is ALWAYS the clean canonical standard answer (本体)
  for (const seed of primarySeeds) addSingleSeedVariants(seed, ctx, push);
  for (const seed of ruleSeeds) addSingleSeedVariants(seed, ctx, push);
  for (const seed of extraSeeds) addSingleSeedVariants(seed, ctx, push);

  return out;
}

function parseDrawingElements(note, parts) {
  const aligned = parseBlankRuleLines(note, parts);
  if (!aligned.aligned) return { elements: [], aligned: false };
  const elements = parts.map((score, i) => {
    const ruleText = aligned.byIndex[i] || '';
    const cleaned = ruleText
      .replace(/\s*[（(]\s*\d+(?:\.\d+)?\s*分\s*[)）]\s*$/, '')
      .replace(/^(?:第\s*[一二三四五六七八九十\d]+\s*(?:空|要素|点)(?:\s*[（(]\s*\d+(?:\.\d+)?\s*分\s*[)）])?\s*[:：]?|[（(]\s*\d+\s*[)）]\s*[:：]?|[①②③④⑤⑥⑦⑧⑨⑩]\s*[:：]?|\d+\s*[.．、:：]\s*)/, '')
      .trim();
    const firstPart = cleaned.split(/[；;。]/)[0].trim();
    const name = firstPart
      .replace(/(?:均)?(?:给|得)\s*(?:满分|\d+(?:\.\d+)?\s*分)$/, '')
      .replace(/即可$/, '')
      .trim() || ('作图要素 ' + (i + 1));
    return { name, score, rule_text: ruleText };
  });
  return { elements, aligned: true };
}

function defaultObjectiveRule(kind, points, partial) {
  if (kind === 0) return '判断正确得 ' + points + ' 分，错误或未答得 0 分。';
  if (kind === 1) return '选对得 ' + points + ' 分，错选或未选得 0 分。';
  return '全选对得 ' + points + ' 分，漏选且无错选得 ' + partial + ' 分，错选或未选得 0 分。';
}

// ---------- Objective Answer Grid (180mm 10-Col x 18mm + BFGRID0 Single Anchor) ----------

function roundCoord4(v) {
  return Math.round(Math.max(0, Math.min(1, Number(v) || 0)) * 10000) / 10000;
}

function roundMm2(v) {
  return Math.round((Number(v) || 0) * 100) / 100;
}

function toAnchorNormRect(rectMm) {
  return {
    nx: roundCoord4((rectMm.x - GRADING_ANCHOR_TL_CENTER_MM[0]) / GRADING_ANCHOR_REF_W_MM),
    ny: roundCoord4((rectMm.y - GRADING_ANCHOR_TL_CENTER_MM[1]) / GRADING_ANCHOR_REF_H_MM),
    nw: roundCoord4(rectMm.w / GRADING_ANCHOR_REF_W_MM),
    nh: roundCoord4(rectMm.h / GRADING_ANCHOR_REF_H_MM)
  };
}

function buildAnchorsMmSpec() {
  return {
    markerSize: GRADING_ANCHOR_SIZE_MM,
    insetMm: GRADING_ANCHOR_INSET_MM,
    topLeftCenter: GRADING_ANCHOR_TL_CENTER_MM.slice(),
    bottomRightCenter: GRADING_ANCHOR_BR_CENTER_MM.slice(),
    refWidth: GRADING_ANCHOR_REF_W_MM,
    refHeight: GRADING_ANCHOR_REF_H_MM
  };
}

function buildFooterQrMmSpec() {
  return {
    x: GRADING_FOOTER_QR_X_MM,
    y: GRADING_FOOTER_QR_Y_MM,
    size: GRADING_QR_SIZE_MM
  };
}

function buildScanSpec() {
  return {
    targetDpi: GRADING_SCAN_DPI,
    qrFormat: 'BF:{paper_id}:{page_no}',
    qrSizeLogicalMm: GRADING_QR_SIZE_MM,
    paperSheetSpec: '8K_260x368_shrink_87.5pct',
    qrSizePhysical8kMm: 13.1,
    fiducial: {
      sizeMm: GRADING_ANCHOR_SIZE_MM,
      insetMm: GRADING_ANCHOR_INSET_MM,
      topLeftCenterMm: GRADING_ANCHOR_TL_CENTER_MM.slice(),
      bottomRightCenterMm: GRADING_ANCHOR_BR_CENTER_MM.slice(),
      frameWidthMm: GRADING_ANCHOR_REF_W_MM,
      frameHeightMm: GRADING_ANCHOR_REF_H_MM
    }
  };
}

function collectChoiceBoxEntries(edition, items) {
  const grading = edition?.grading || null;
  const entries = [];
  for (let i = 0; i < items.length; i++) {
    const q = items[i].q;
    const rule = grading?.items?.[q.id] || null;
    const kind = rule ? rule.kind : basketSection(q);
    if (kind === 0 || kind === 1 || kind === 2) {
      const type = kind === 0 ? 'judge' : kind === 1 ? 'single' : 'multiple';
      const score = rule?.points ?? (kind === 0 ? (grading?.judgment ?? 1) : kind === 1 ? (grading?.single ?? 2) : (grading?.multi ?? 3));
      const rawAns = htmlToPlainText(items[i].answer || '');
      const answer = kind === 0 ? normalizeJudgmentAnswer(rawAns) : normalizeChoiceAnswer(rawAns);
      entries.push({ no: i + 1, qIndex: i, qId: q.id || '', kind, type, score, answer });
    }
  }
  return entries;
}

function computeChoiceBoxGridGeometry(count) {
  if (!count || count <= 0) return null;
  const cols = CHOICE_GRID_COLS;
  const rows = Math.ceil(count / cols);
  return {
    count,
    rows,
    cols,
    cellWMm: CHOICE_GRID_CELL_W_MM,
    titleHMm: CHOICE_GRID_TITLE_H_MM,
    labelHMm: CHOICE_GRID_HEADER_H_MM,
    cellHMm: CHOICE_GRID_WRITE_H_MM
  };
}

function buildChoiceBoxInstruction(entries) {
  const kinds = new Set(entries.map(e => e.kind));
  const hints = [];
  if (kinds.has(1)) hints.push('单选填 A-D');
  if (kinds.has(2)) hints.push('多选如 AC');
  if (kinds.has(0)) hints.push('判断填 T/F');
  const detail = hints.length ? hints.join('，') : '每格手写大写字母';
  return `选择题答题栏（${detail}，请工整填写于框内）`;
}

function buildChoiceAnswerBoxHtml(entries) {
  if (!entries || !entries.length) return '';
  const geom = computeChoiceBoxGridGeometry(entries.length);
  const tip = buildChoiceBoxInstruction(entries);
  const trs = [
    `<tr class="ans-title-row"><th colspan="${geom.cols}" class="ans-title-cell"><span class="ans-grid-probe">${GRID_ANCHOR_TOKEN}</span><span class="ans-title-text">${escapeDoc(tip)}</span></th></tr>`
  ];
  for (let r = 0; r < geom.rows; r++) {
    const noCells = [];
    const writeCells = [];
    for (let c = 0; c < geom.cols; c++) {
      const idx = r * geom.cols + c;
      const entry = idx < entries.length ? entries[idx] : null;
      if (entry) {
        noCells.push(`<th class="ans-no-cell">${entry.no}</th>`);
        writeCells.push(`<td class="ans-write-cell" data-qno="${entry.no}" data-qid="${escapeDoc(entry.qId)}" data-qtype="${entry.type}" data-score="${entry.score}" data-row="${r}" data-col="${c}"></td>`);
      } else {
        const isAfterLast = idx === entries.length;
        noCells.push(`<th class="ans-pad-empty${isAfterLast ? ' ans-after-last-no' : ''}"></th>`);
        writeCells.push(`<td class="ans-pad-empty${isAfterLast ? ' ans-after-last-write' : ''}"></td>`);
      }
    }
    trs.push(`<tr class="ans-no-row">${noCells.join('')}</tr>`);
    trs.push(`<tr class="ans-write-row" data-ans-row="${r}">${writeCells.join('')}</tr>`);
  }
  return `<div class="choice-ans-wrap" id="bf-choice-answer-box">
    <table class="choice-ans-grid" id="bf-choice-ans-grid">${trs.join('')}</table>
  </div>`;
}

// Derives exact handwriting cell coordinates from gridOriginMm = (originX, titleRowTopY)
// where Title Row = 5.0mm, Number Row = 5.0mm, Handwriting Row = 10.0mm, Cell Width = 18.0mm.
function buildAnswerBoxFromGridOriginMm(entries, pageNo, gridOriginXMm, gridOriginYMm) {
  if (!entries || !entries.length) return null;
  const geom = computeChoiceBoxGridGeometry(entries.length);
  const cellWMm = geom.cellWMm;       // 18.0mm
  const titleHMm = geom.titleHMm;     // 5.0mm
  const headerHMm = geom.labelHMm;    // 5.0mm
  const cellHMm = geom.cellHMm;       // 10.0mm
  const writeRow0TopMm = gridOriginYMm + titleHMm + headerHMm; // +10.0mm
  const rowGapMm = geom.rows > 1 ? headerHMm : 0;
  const pitchYMm = cellHMm + headerHMm; // 15.0mm
  const totalWMm = geom.cols * cellWMm; // 180.0mm
  const totalWriteHMm = geom.rows * cellHMm + Math.max(0, geom.rows - 1) * headerHMm;
  const gridOriginMm = { x: roundMm2(gridOriginXMm), y: roundMm2(gridOriginYMm) };

  const cells = entries.map((e, idx) => {
    const r = Math.floor(idx / geom.cols);
    const c = idx % geom.cols;
    const cxMm = gridOriginXMm + c * cellWMm;
    const cyMm = writeRow0TopMm + r * pitchYMm;
    const rectMm = { x: roundMm2(cxMm), y: roundMm2(cyMm), w: roundMm2(cellWMm), h: roundMm2(cellHMm) };
    const normRect = toAnchorNormRect(rectMm);
    return {
      no: e.no,
      qIndex: e.no,
      qId: e.qId || '',
      type: e.type,
      qType: e.type,
      answer: e.answer || '',
      score: e.score,
      row: r,
      col: c,
      rectMm,
      normRect,
      x: roundCoord4(cxMm / 210),
      y: roundCoord4(cyMm / 297),
      w: roundCoord4(cellWMm / 210),
      h: roundCoord4(cellHMm / 297),
      bbox_mm: rectMm
    };
  });

  const boxRectMm = { x: roundMm2(gridOriginXMm), y: roundMm2(writeRow0TopMm), w: roundMm2(totalWMm), h: roundMm2(totalWriteHMm) };
  const boxNormRect = toAnchorNormRect(boxRectMm);
  const bbox = {
    x: roundCoord4(gridOriginXMm / 210),
    y: roundCoord4(writeRow0TopMm / 297),
    w: roundCoord4(totalWMm / 210),
    h: roundCoord4(totalWriteHMm / 297)
  };
  return {
    anchorToken: GRID_ANCHOR_TOKEN,
    page: pageNo,
    coord_system: 'anchor_norm_rect',
    gridOriginMm,
    columnsPerRow: geom.cols,
    rows: geom.rows,
    cols: geom.cols,
    cellWidthMm: cellWMm,
    titleHeightMm: titleHMm,
    headerHeightMm: headerHMm,
    writeBoxHeightMm: cellHMm,
    cell_w_mm: cellWMm,
    title_h_mm: titleHMm,
    header_h_mm: headerHMm,
    cell_h_mm: cellHMm,
    row_gap_mm: roundMm2(rowGapMm),
    rectMm: boxRectMm,
    normRect: boxNormRect,
    x: bbox.x,
    y: bbox.y,
    w: bbox.w,
    h: bbox.h,
    bbox,
    bbox_mm: boxRectMm,
    cell_w: roundCoord4(cellWMm / 210),
    cell_h: roundCoord4(cellHMm / 297),
    col_gap: 0,
    row_gap: roundCoord4(rowGapMm / 297),
    question_nos: entries.map(e => e.no),
    cells
  };
}

// Keep alias compatible with callers passing writeRow0TopMm directly
function buildAnswerBoxFromOriginMm(entries, pageNo, originXMm, writeRow0TopMm) {
  return buildAnswerBoxFromGridOriginMm(entries, pageNo, originXMm, writeRow0TopMm - CHOICE_GRID_TITLE_H_MM - CHOICE_GRID_HEADER_H_MM);
}

function buildFallbackAnswerBoxMeta(entries, pageNo = 1) {
  return buildAnswerBoxFromGridOriginMm(entries, pageNo, GRADING_PAGE_MARGIN_SIDE_MM, 170);
}

function buildGradingPack(edition, items, opts = {}) {
  const grading = edition?.grading || null;
  const studentIdDigits = normalizeStudentIdDigits(opts.studentIdDigits);
  const paperId = opts.paperId || '';
  const questionPages = opts.questionPages || null;
  const pageCount = Number.isInteger(opts.pageCount) && opts.pageCount > 0 ? opts.pageCount : null;
  const generatedAt = opts.generatedAt || isoLocalTimestamp();
  const choiceEntries = collectChoiceBoxEntries(edition, items);
  const hasAnswerBox = choiceEntries.length > 0;
  const answerBox = hasAnswerBox
    ? (opts.answerBox || buildFallbackAnswerBoxMeta(choiceEntries, pageCount || 1))
    : null;

  let computedTotal = 0;
  const questions = items.map((item, idx) => {
    const q = item.q;
    const no = idx + 1;
    const rule = grading?.items?.[q.id] || null;
    const kind = rule ? rule.kind : basketSection(q);
    const page = questionPages ? (questionPages.get ? questionPages.get(idx) : questionPages[idx]) ?? null : null;
    const rawAnswerText = htmlToPlainText(item.answer || '');

    if (kind === 0 || kind === 1 || kind === 2) {
      const points = rule?.points ?? (kind === 0 ? (grading?.judgment ?? 1) : kind === 1 ? (grading?.single ?? 2) : (grading?.multi ?? 3));
      const partial = kind === 2 ? (grading?.partial ?? 1) : 0;
      computedTotal += points;
      let type = 'single', answer = '';
      if (kind === 0) {
        type = 'judge';
        answer = normalizeJudgmentAnswer(rawAnswerText);
      } else if (kind === 1) {
        type = 'single';
        answer = normalizeChoiceAnswer(rawAnswerText);
      } else {
        type = 'multiple';
        const norm = normalizeChoiceAnswer(rawAnswerText);
        answer = norm ? norm.split('') : [];
      }
      const entry = {
        no,
        type,
        score: points,
        page,
        answer,
        answer_source: 'answer_box',
        grading_rule: rule ? markScheme(grading, q.id) : defaultObjectiveRule(kind, points, partial)
      };
      if (kind === 2) entry.partial_score = partial;
      return entry;
    }

    const stemBlankCtxs = extractStemBlankContexts(q);
    let parts = Array.isArray(rule?.parts) && rule.parts.length ? rule.parts.slice() : null;
    if (!parts && typeof document !== 'undefined' && typeof initialBlankScores === 'function' && typeof parseBlankScores === 'function') {
      const fallback = parseBlankScores(initialBlankScores(q));
      parts = fallback && fallback.length ? fallback : null;
    }
    if (!parts && stemBlankCtxs.length > 0) {
      parts = Array.from({ length: stemBlankCtxs.length }, () => 1);
    }
    if (!parts || !parts.length) parts = [rule?.points || 1];
    const points = rule?.points ?? parts.reduce((a, b) => a + b, 0);
    computedTotal += points;
    const note = String(rule?.note || '').trim();
    const isDrawing = Boolean(rule?.isDrawing || q.type === '作图题' || q.category === '作图题');

    if (isDrawing) {
      const parsed = parseDrawingElements(note, parts);
      const entry = {
        no,
        type: 'drawing',
        score: points,
        page,
        elements: parsed.elements
      };
      if (!parsed.aligned || !parsed.elements.length) entry.rule_text = note;
      if (rawAnswerText) entry.reference_answer = rawAnswerText;
      return entry;
    }

    const parsed = parseBlankRuleLines(note, parts);
    const refSlots = splitReferenceAnswerByBlanks(q, item.answer || rawAnswerText, parts.length);
    const stemCountAligned = stemBlankCtxs.length === parts.length;
    const blanks = parts.map((blankScore, i) => {
      const ruleText = parsed.aligned ? (parsed.byIndex[i] || '') : '';
      const fromRule = ruleText ? extractAcceptedAnswers(ruleText, blankScore) : [];
      const refSlot = refSlots[i] || { primary: '', extras: [], ctx: stemBlankCtxs[i] || null };
      // When custom #grading merged blanks so parts.length !== stemBlanks.length, prefer explicit ruleText
      // answers first so indices stay locked to rule_text, falling back to refSlot only when fromRule is empty.
      const effectiveSlot = (!stemCountAligned && parsed.aligned && fromRule.length > 0)
        ? { primary: '', extras: [], ctx: refSlot.ctx }
        : refSlot;
      return {
        index: i + 1,
        score: blankScore,
        accepted: buildBlankAcceptedList(effectiveSlot, fromRule),
        rule_text: ruleText
      };
    });
    const entry = {
      no,
      type: 'blank',
      score: points,
      page,
      blanks
    };
    if (note && !parsed.aligned) entry.rule_text = note;
    if (rawAnswerText) entry.reference_answer = rawAnswerText;
    return entry;
  });

  const anchorsMm = buildAnchorsMmSpec();
  const footerQrMm = buildFooterQrMmSpec();
  const scanSpec = buildScanSpec();
  const objectiveAnswerGrid = answerBox ? {
    anchorToken: GRID_ANCHOR_TOKEN,
    page: answerBox.page,
    gridOriginMm: answerBox.gridOriginMm || {
      x: roundMm2((answerBox.rectMm || answerBox.bbox_mm).x),
      y: roundMm2((answerBox.rectMm || answerBox.bbox_mm).y - CHOICE_GRID_TITLE_H_MM - CHOICE_GRID_HEADER_H_MM)
    },
    columnsPerRow: answerBox.cols,
    cellWidthMm: answerBox.cell_w_mm,
    titleHeightMm: answerBox.title_h_mm ?? CHOICE_GRID_TITLE_H_MM,
    headerHeightMm: answerBox.header_h_mm ?? CHOICE_GRID_HEADER_H_MM,
    writeBoxHeightMm: answerBox.cell_h_mm,
    rectMm: answerBox.rectMm || answerBox.bbox_mm,
    normRect: answerBox.normRect || toAnchorNormRect(answerBox.rectMm || answerBox.bbox_mm),
    cells: answerBox.cells
  } : null;

  return {
    schemaVersion: GRADING_SCHEMA_VERSION,
    examId: paperId,
    scanDpiRecommended: GRADING_SCAN_DPI,
    scanSpec,
    pageCount,
    totalPages: pageCount,
    anchorsMm,
    footerQrMm,
    paper: {
      paper_id: paperId,
      title: edition?.name || '未命名试卷',
      usage: edition?.purpose || '',
      total_score: grading?.total ?? computedTotal,
      page_count: pageCount,
      student_id_digits: studentIdDigits,
      layout: {
        logical_page_mm: [210, 297],
        body_mm: [GRADING_BODY_WIDTH_MM, GRADING_BODY_HEIGHT_MM],
        margins_mm: {
          top: GRADING_PAGE_MARGIN_TOP_MM,
          bottom: GRADING_PAGE_MARGIN_BOTTOM_MM,
          left: GRADING_PAGE_MARGIN_SIDE_MM,
          right: GRADING_PAGE_MARGIN_SIDE_MM
        },
        scan_dpi: GRADING_SCAN_DPI,
        paper_sheet_spec: '8K_260x368_shrink_87.5pct',
        qr_position: 'bottom-right',
        qr_size_mm: GRADING_QR_SIZE_MM,
        anchor_size_mm: GRADING_ANCHOR_SIZE_MM,
        anchor_inset_mm: GRADING_ANCHOR_INSET_MM,
        anchors_mm: anchorsMm,
        footer_qr_mm: footerQrMm
      },
      generated_at: generatedAt
    },
    answer_box: answerBox,
    objectiveAnswerGrid,
    questions
  };
}

// ---------- Continuous A4 Student Layout & Vector PDF Post-Stamping ----------

const GRADING_A4_PRINT_CSS = `
@page { size: A4; margin: 0; }
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; background: #fff; color: #111; }
body { font: 12pt/1.45 "Songti SC", "SimSun", "Noto Serif CJK SC", "Times New Roman", serif; -webkit-print-color-adjust: exact; print-color-adjust: exact; }
.grading-sheet { width: 210mm; height: 297mm; padding: 14mm 15mm 22mm 15mm; display: flex; flex-direction: column; overflow: hidden; page-break-after: always; break-after: page; position: relative; background: #fff; }
.grading-sheet:last-child { page-break-after: auto; break-after: auto; }
.hdr-school { flex: none; font-size: 9pt; color: #333; line-height: 1.32; padding-bottom: 1.2mm; margin-bottom: 2mm; border-bottom: 0.5pt solid #999; text-align: center; }
.sheet-content { flex: 1; min-height: 0; overflow: hidden; }
.sheet-stamp { position: absolute; inset: 0; pointer-events: none; z-index: 5; }
.reg-mark { position: absolute; width: 4mm; height: 4mm; background: #000; }
.reg-tl { top: 6mm; left: 6mm; }
.reg-tr { top: 6mm; right: 6mm; }
.reg-bl { bottom: 6mm; left: 6mm; }
.reg-br { bottom: 6mm; right: 6mm; }
.stamp-ftr { position: absolute; left: 15mm; right: 15mm; bottom: 5mm; height: 15.2mm; display: flex; align-items: flex-end; justify-content: flex-end; background: transparent; }
.ftr-page-no { position: absolute; left: 0; right: 0; bottom: 1.8mm; text-align: center; font-size: 8.5pt; color: #333; line-height: 1.2; pointer-events: none; white-space: nowrap; }
.ftr-qr-wrap { position: relative; z-index: 1; display: inline-flex; align-items: flex-end; gap: 3mm; height: 15mm; background: #fff; }
.ftr-qr-code { font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, "Courier New", monospace; font-size: 7pt; line-height: 1.2; color: #111; padding-bottom: 1mm; white-space: nowrap; }
.ftr-qr-wrap svg { display: block; width: 15mm; height: 15mm; flex: none; shape-rendering: crispEdges; }
.paper-head { text-align: center; margin-bottom: 5pt; padding-bottom: 4pt; border-bottom: 0.7pt solid #999; }
.paper-head h1 { margin: 0 0 2pt; font-size: 14.5pt; font-weight: 700; line-height: 1.25; }
.paper-meta { margin: 1pt 0 3pt; font-size: 9pt; color: #333; }
.student-bar { display: flex; flex-wrap: wrap; align-items: center; justify-content: center; gap: 3mm 5.5mm; margin: 3pt 0 0.5pt; font-size: 9.8pt; color: #111; }
.student-bar.two-row { flex-direction: column; gap: 2mm; }
.student-fields { display: inline-flex; align-items: baseline; gap: 5.5mm; }
.fill-line { display: inline-block; width: 23mm; border-bottom: 0.7pt solid #111; transform: translateY(-0.6mm); }
.id-box-group { display: inline-flex; align-items: center; gap: 1.8mm; }
.id-label { margin-right: 0.5mm; }
.id-box { display: inline-block; width: 6mm; height: 6mm; border: 0.35mm solid #000; background: #fff; }
.section-head { margin: 6pt 0 3pt; font-size: 12pt; font-weight: 700; font-family: "Kaiti SC", "KaiTi", "STKaiti", serif; }
.q-item { margin: 0 0 4.5pt; }
.q-item.q-part-head { margin-bottom: 1.5pt; }
.q-item.q-part-cont { margin: 0 0 1.5pt; }
.q-item.q-part-last { margin-bottom: 4.5pt; }
.q-body { display: block; }
.q-no { font-weight: 600; float: left; margin-right: 2.5pt; }
.doc p { margin: 0 0 2pt; white-space: pre-wrap; overflow-wrap: anywhere; }
.doc p:first-child { display: inline; }
.doc p:first-child::after { content: ""; display: block; }
.q-part-cont .doc p:first-child { display: block; }
.q-part-cont .doc p:first-child::after { display: none; }
.doc p.fig-center-block { display: block !important; text-align: center; margin: 2.5pt 0; }
.doc p.fig-center-block::after { display: none !important; }
.doc p.fig-center-block img { display: inline-block; margin: 0 auto; }
.doc img { display: block; max-width: 165mm; max-height: 85mm; height: auto; margin: 2pt 0; object-fit: contain; }
.doc table { border-collapse: collapse; margin: 2.5pt 0; max-width: 100%; }
.doc table:not(.opt-table):not(.fig-wrap-table):not(.fig-pair-table) td,
.doc table:not(.opt-table):not(.fig-wrap-table):not(.fig-pair-table) th { border: 0.6pt solid #444; padding: 1.8pt 5pt; font-size: 9.5pt; }
.doc table.opt-table { width: 100%; table-layout: fixed; margin: 0.8pt 0 1pt; border: 0; }
.doc table.opt-table td { border: 0; padding: 0.5pt 5pt 0.5pt 0; font-size: 12pt; line-height: 1.43; vertical-align: top; }
.doc table.fig-wrap-table { width: 100%; table-layout: fixed; margin: 1pt 0 1.5pt; border: 0; }
.doc table.fig-wrap-table td { border: 0; padding: 0; vertical-align: top; }
.doc table.fig-wrap-table td.fig-wrap-text { padding-right: 2.5mm; }
.doc table.fig-wrap-table td.fig-wrap-text p { display: block !important; margin: 0 0 1.8pt; }
.doc table.fig-wrap-table td.fig-wrap-text p::after { display: none !important; }
.doc table.fig-wrap-table td.fig-wrap-media { text-align: right; vertical-align: middle; }
.doc table.fig-wrap-table td.fig-wrap-media img { display: inline-block; margin: 0 0 0 auto; }
.doc table.fig-pair-table { width: 100%; table-layout: fixed; margin: 2pt 0; border: 0; }
.doc table.fig-pair-table td { width: 50%; border: 0; padding: 0 1.5mm; text-align: center; vertical-align: middle; }
.doc table.fig-pair-table td img { display: inline-block; margin: 0 auto; }
.choice-ans-wrap { margin: 3mm 0 3.5mm; width: 180mm; }
.choice-ans-grid { width: 180mm; border-collapse: collapse; table-layout: fixed; margin: 0; }
.ans-title-cell { height: 5mm; padding: 0 1mm 0 0.35mm; text-align: left; vertical-align: middle; background: #fafafa; border: 0.75pt solid #000; line-height: 1; white-space: nowrap; overflow: hidden; }
.ans-grid-probe { font-family: Consolas, "Courier New", monospace; font-size: 2pt; color: #efefef; line-height: 1; margin-right: 0.6mm; user-select: none; }
.ans-title-text { font-size: 8.5pt; font-weight: 700; color: #111; vertical-align: middle; }
.ans-no-cell { width: 18mm; min-width: 18mm; max-width: 18mm; height: 5mm; padding: 0; text-align: center; vertical-align: middle; font-size: 9pt; font-weight: 700; font-family: "Times New Roman", serif; color: #111; background: #f2f2f2; border: 0.75pt solid #000; border-bottom: 1pt solid #000; line-height: 1; }
.ans-write-cell { width: 18mm; min-width: 18mm; max-width: 18mm; height: 10mm; padding: 0; background: #fff; border: 1pt solid #000; text-align: center; vertical-align: middle; }
.ans-pad-empty { width: 18mm; min-width: 18mm; max-width: 18mm; padding: 0; background: transparent; border: 0; }
.ans-after-last-no { border-left: 0.75pt solid #000; }
.ans-after-last-write { border-left: 1pt solid #000; }
`;

function buildStudentLeadHtml(edition, items, studentIdDigits) {
  const digits = normalizeStudentIdDigits(studentIdDigits);
  const grading = edition?.grading;
  const meta = [edition?.purpose, grading ? `满分 ${grading.total} 分` : '', `${items.length} 道题`].filter(Boolean).join('　·　');
  const boxes = Array.from({ length: digits }, () => '<span class="id-box"></span>').join('');
  const twoRow = digits > 8 ? ' two-row' : '';
  return `<header class="paper-head">
    <h1>${escapeDoc(edition?.name || '生物试卷')}</h1>
    ${meta ? `<p class="paper-meta">${escapeDoc(meta)}</p>` : ''}
    <div class="student-bar${twoRow}">
      <span class="student-fields"><span>班级：<span class="fill-line"></span></span><span>姓名：<span class="fill-line"></span></span></span>
      <span class="id-box-group"><span class="id-label">学号：</span>${boxes}</span>
    </div>
  </header>`;
}

function applyImageBoxAttrs(img, box) {
  img.setAttribute('width', String(box.widthPx));
  img.setAttribute('height', String(box.heightPx));
  img.setAttribute('data-w-mm', box.widthMm.toFixed(2));
  img.setAttribute('data-h-mm', box.heightMm.toFixed(2));
  img.setAttribute('data-max-w-mm', String(box.maxWidthMm));
  img.setAttribute('data-max-h-mm', String(box.maxHeightMm));
  img.setAttribute('data-tier', box.tier || 'choice');
  img.setAttribute('style', `width:${box.widthMm.toFixed(2)}mm;height:${box.heightMm.toFixed(2)}mm`);
}

async function inlineGradingPictures(html, isSubjectiveOrOpts = false) {
  if (!html) return '';
  if (typeof document === 'undefined') return safeHtml(html);
  const opts = (typeof isSubjectiveOrOpts === 'boolean')
    ? { isSubjective: isSubjectiveOrOpts, tier: isSubjectiveOrOpts ? 'subjective' : 'choice', manual: false }
    : (isSubjectiveOrOpts || {});
  const tier = opts.tier || (opts.isSubjective ? 'subjective' : 'choice');
  const manual = Boolean(opts.manualTier || opts.manual);

  const tpl = document.createElement('template');
  tpl.innerHTML = safeHtml(html);
  for (const img of tpl.content.querySelectorAll('img')) {
    const src = img.getAttribute('src');
    if (!src) continue;
    try {
      const asset = await resolvePrintImageAsset(src, {
        isSubjective: Boolean(opts.isSubjective),
        tier,
        manual
      });
      img.setAttribute('src', asset.url);
      img.setAttribute('data-orig-w', String(asset.naturalWidth));
      img.setAttribute('data-orig-h', String(asset.naturalHeight));
      img.setAttribute('data-dpi-x', String(asset.dpiX));
      img.setAttribute('data-dpi-y', String(asset.dpiY));
      img.setAttribute('data-has-dpi', asset.hasPhysicalDpi ? '1' : '0');
      img.setAttribute('data-manual-tier', manual ? '1' : '0');
      applyImageBoxAttrs(img, asset);
    } catch {
      if (libraryPicture(src)) img.setAttribute('src', await inlinePicture(src));
    }
  }
  return tpl.innerHTML;
}

function optionVisualUnits(text) {
  let u = 0;
  for (const ch of String(text || '').trim()) {
    u += ch.charCodeAt(0) > 127 ? 1 : 0.55;
  }
  return u;
}

function compactChoiceOptionsHtml(html) {
  if (typeof document === 'undefined' || !html) return html;
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const ps = Array.from(tpl.content.children).filter(el => el.tagName === 'P');
  const seen = new Set();
  for (const p of ps) {
    if (seen.has(p) || !/^\s*A\s*[.．、]/.test(p.textContent)) continue;
    const group = [p];
    let nxt = p.nextElementSibling;
    while (nxt && nxt.tagName === 'P' && /^\s*[B-H]\s*[.．、]/.test(nxt.textContent)) {
      group.push(nxt);
      nxt = nxt.nextElementSibling;
    }
    group.forEach(x => seen.add(x));
    if (group.length < 2 || group.some(x => x.querySelector('img,table,br'))) continue;
    const maxUnits = Math.max(...group.map(x => optionVisualUnits(x.textContent)));
    if (maxUnits > 21.4) continue;
    const cols = (group.length === 4 && maxUnits <= 9.8) ? 4 : 2;
    const tbl = document.createElement('table');
    tbl.className = 'opt-table';
    tbl.setAttribute('data-opt-grid', String(cols));
    for (let i = 0; i < group.length; i += cols) {
      const tr = document.createElement('tr');
      for (let c = 0; c < cols; c++) {
        const td = document.createElement('td');
        if (i + c < group.length) td.innerHTML = group[i + c].innerHTML;
        tr.appendChild(td);
      }
      tbl.appendChild(tr);
    }
    p.before(tbl);
    group.forEach(x => x.remove());
  }
  return tpl.innerHTML;
}

// Benchmark-aligned figure layout shared by BOTH PDF and Word (.docx):
// - Dual standalone figures -> side-by-side 2-col row (~45% width each)
// - Single moderate-width figure -> right-aligned (~1/3–45% width) with options/sub-questions wrapping on left
// - Wide composite / tall vertical-stack / manual [大] figure -> centered block using 3-tier computeImageBoxMm
function layoutQuestionFiguresHtml(html, opts = {}) {
  if (typeof document === 'undefined' || !html) return html;
  const bodyWMm = opts.bodyWidthMm ?? GRADING_BODY_WIDTH_MM; // 180mm
  const isChoice = Boolean(opts.isChoice);
  const tier = opts.tier || (isChoice ? 'choice' : 'subjective');
  const manualTier = Boolean(opts.manualTier);
  const tpl = document.createElement('template');
  tpl.innerHTML = html;

  const standaloneImgs = el => {
    if (!el || (el.tagName !== 'P' && el.tagName !== 'DIV')) return null;
    if ((el.textContent || '').trim() !== '' || el.querySelector('table')) return null;
    const imgs = Array.from(el.querySelectorAll('img'));
    return imgs.length > 0 ? imgs : null;
  };
  const reboxImg = (img, customMaxW = null, customMaxH = null) => {
    const origW = Number(img.getAttribute('data-orig-w')) || 320;
    const origH = Number(img.getAttribute('data-orig-h')) || 240;
    const dpiX = Number(img.getAttribute('data-dpi-x')) || 150;
    const dpiY = Number(img.getAttribute('data-dpi-y')) || 150;
    const hasPhysicalDpi = img.getAttribute('data-has-dpi') === '1';
    const box = computeImageBoxMm(origW, origH, tier, {
      manual: manualTier,
      maxWidthMm: customMaxW,
      maxHeightMm: customMaxH,
      dpiX,
      dpiY,
      hasPhysicalDpi
    });
    applyImageBoxAttrs(img, box);
    return box;
  };
  const isTextPara = el => el && el.tagName === 'P' && !el.querySelector('img,table') && (el.textContent || '').trim() !== '';

  const kids = () => Array.from(tpl.content.children);
  let idx = 0;
  while (idx < kids().length) {
    const list = kids();
    const cur = list[idx];
    const imgs = standaloneImgs(cur);
    if (!imgs) { idx++; continue; }

    // 1. Two figures in one <p> or two consecutive standalone <p><img></p> -> side-by-side (~45% width each)
    const nextImgs = (imgs.length === 1 && idx + 1 < list.length) ? standaloneImgs(list[idx + 1]) : null;
    if (imgs.length === 2 || (imgs.length === 1 && nextImgs && nextImgs.length === 1)) {
      const pair = imgs.length === 2 ? imgs : [imgs[0], nextImgs[0]];
      const pairMaxW = Math.round(bodyWMm * 0.45); // 81mm
      const pairMaxH = tier === 'subjective-lg' ? 78 : (tier === 'choice' ? 48 : 64);
      const tbl = document.createElement('table');
      tbl.className = 'fig-pair-table';
      tbl.setAttribute('data-fig-pair', '2');
      const tr = document.createElement('tr');
      for (const im of pair) {
        const clone = im.cloneNode(true);
        reboxImg(clone, pairMaxW, pairMaxH);
        const td = document.createElement('td');
        td.appendChild(clone);
        tr.appendChild(td);
      }
      tbl.appendChild(tr);
      cur.before(tbl);
      if (nextImgs && nextImgs.length === 1) list[idx + 1].remove();
      cur.remove();
      idx++;
      continue;
    }

    // 2. Single standalone figure: wrap on right if moderate width and not manually forced to large/full tier
    if (imgs.length === 1) {
      const im = imgs[0];
      const origW = Number(im.getAttribute('data-orig-w')) || 320;
      const origH = Number(im.getAttribute('data-orig-h')) || 240;
      const curW = Number(im.getAttribute('data-w-mm')) || 70;
      const aspectWH = origW / Math.max(1, origH);
      const aspectHW = origH / Math.max(1, origW);
      const manualExpanded = manualTier && (tier === 'subjective-lg' || (isChoice && tier === 'subjective'));
      const canWrapRight = !manualExpanded &&
        aspectHW <= GRADING_TALL_ASPECT_THRESHOLD &&
        aspectWH <= 2.25 &&
        (isChoice ? curW <= 95 : curW <= 84);

      if (canWrapRight) {
        const afterParas = [];
        for (let j = idx + 1; j < list.length && isTextPara(list[j]); j++) afterParas.push(list[j]);
        const beforeOptParas = [];
        if (!afterParas.length && isChoice) {
          for (let j = idx - 1; j >= 1 && isTextPara(list[j]) && /^\s*[A-H]\s*[.．、]/.test(list[j].textContent); j--) {
            beforeOptParas.unshift(list[j]);
          }
        }
        const candidates = afterParas.length ? afterParas : beforeOptParas;
        if (candidates.length > 0) {
          const clone = im.cloneNode(true);
          const wrapMaxW = isChoice ? 78 : 80; // ~43% of 180mm
          const wrapMaxH = isChoice ? 48 : 62;
          const box = reboxImg(clone, wrapMaxW, wrapMaxH);
          const rightMm = Math.min(83, Math.max(28, Math.ceil(box.widthMm + 2.5)));
          const leftMm = bodyWMm - rightMm;

          let wrapped = [];
          if (isChoice) {
            wrapped = candidates;
          } else {
            const charsPerLine = Math.max(12, Math.floor(leftMm / 4.28));
            let accHMm = 0;
            for (const p of candidates) {
              if (wrapped.length > 0 && accHMm >= box.heightMm * 0.9) break;
              const lines = Math.max(1, Math.ceil(optionVisualUnits(p.textContent) / charsPerLine));
              accHMm += lines * 6.15 + 1.1;
              wrapped.push(p);
            }
          }

          if (wrapped.length > 0) {
            const tbl = document.createElement('table');
            tbl.className = 'fig-wrap-table';
            tbl.setAttribute('data-fig-wrap', '1');
            tbl.setAttribute('data-left-mm', String(leftMm));
            tbl.setAttribute('data-right-mm', String(rightMm));
            const tr = document.createElement('tr');
            const tdLeft = document.createElement('td');
            tdLeft.className = 'fig-wrap-text';
            tdLeft.setAttribute('style', `width:${leftMm}mm`);
            for (const p of wrapped) tdLeft.appendChild(p.cloneNode(true));
            const tdRight = document.createElement('td');
            tdRight.className = 'fig-wrap-media';
            tdRight.setAttribute('style', `width:${rightMm}mm`);
            tdRight.appendChild(clone);
            tr.appendChild(tdLeft);
            tr.appendChild(tdRight);
            tbl.appendChild(tr);

            const anchorNode = afterParas.length ? cur : wrapped[0];
            anchorNode.before(tbl);
            cur.remove();
            wrapped.forEach(p => p.remove());
            idx = kids().indexOf(tbl) + 1;
            continue;
          }
        }
      }

      // 3. Centered figure block using exact 3-tier + tall-stack rules
      reboxImg(im);
      cur.classList.add('fig-center-block');
      cur.setAttribute('data-fig-center', '1');
    }
    idx++;
  }

  if (isChoice && !tpl.content.querySelector('table.fig-wrap-table')) {
    return compactChoiceOptionsHtml(tpl.innerHTML);
  }
  return tpl.innerHTML;
}

function splitQuestionHtmlChunks(faceHtml, isChoice = false) {
  if (typeof document === 'undefined' || !faceHtml || isChoice) return [faceHtml];
  const tpl = document.createElement('template');
  tpl.innerHTML = faceHtml;
  const nodes = Array.from(tpl.content.childNodes).filter(n =>
    n.nodeType === 1 || (n.nodeType === 3 && n.textContent.trim())
  );
  if (nodes.length <= 1) return [faceHtml];

  const isLayoutMedia = n => n.nodeType === 1 && (
    n.tagName === 'TABLE' ||
    n.tagName === 'IMG' ||
    n.hasAttribute?.('data-fig-center') ||
    Boolean(n.querySelector?.('img,table'))
  );

  const subStartRe = /^\s*(?:[（(]\s*\d{1,2}\s*[)）]|[①②③④⑤⑥⑦⑧⑨⑩]|[ⅠⅡⅢⅣⅤ]\s*[.．、])/;
  const groups = [[]];
  for (const node of nodes) {
    const txt = (node.textContent || '').trim();
    const cur = groups[groups.length - 1];
    const startsSub = node.nodeType === 1 && subStartRe.test(txt);
    const curHasMedia = cur.some(isLayoutMedia);
    if (cur.length > 0 && (startsSub || curHasMedia)) {
      groups.push([node]);
    } else {
      cur.push(node);
    }
  }
  if (groups.length <= 1) return [faceHtml];

  return groups.map(g => {
    const wrap = document.createElement('div');
    for (const n of g) wrap.appendChild(n.cloneNode(true));
    return wrap.innerHTML;
  });
}

async function prepareStudentFlowUnits(edition, items) {
  const grading = edition?.grading;
  const headings = new Map(paperSections(items.map(it => it.q), grading).map(g => [g.questions[0].id, g.title]));
  const choiceEntries = collectChoiceBoxEntries(edition, items);
  const lastChoiceIdx = choiceEntries.length ? choiceEntries[choiceEntries.length - 1].qIndex : -1;
  const units = [];

  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    if (headings.has(item.q.id)) {
      const title = headings.get(item.q.id);
      units.push({ kind: 'section', title, html: `<h2 class="section-head">${escapeDoc(title)}</h2>` });
    }
    const rule = grading?.items?.[item.q.id];
    const qKind = rule ? rule.kind : basketSection(item.q);
    const isChoice = qKind < 3;
    const { tier, manual } = questionImageTier(edition, item.q, !isChoice);
    const baseFace = isChoice
      ? blankPrintedAnswer(scoredFace(item.q, grading), shortAnswer(item.answer))
      : scoredFace(item.q, grading);
    const inlinedFace = await inlineGradingPictures(baseFace, {
      isSubjective: !isChoice,
      tier,
      manualTier: manual
    });
    const face = layoutQuestionFiguresHtml(inlinedFace, {
      bodyWidthMm: GRADING_BODY_WIDTH_MM,
      isChoice,
      tier,
      manualTier: manual
    });
    const prefix = `${i + 1}．` + (qKind === 3 && rule ? `（${rule.points}分）` : '');

    const chunks = splitQuestionHtmlChunks(face, isChoice);
    if (chunks.length > 1) {
      for (let ci = 0; ci < chunks.length; ci++) {
        const cls = ci === 0 ? 'q-item q-part-head' : (ci === chunks.length - 1 ? 'q-item q-part-cont q-part-last' : 'q-item q-part-cont');
        const inner = ci === 0
          ? `<div class="q-body"><span class="q-no">${escapeDoc(prefix)}</span><div class="doc">${chunks[ci]}</div></div>`
          : `<div class="q-body"><div class="doc">${chunks[ci]}</div></div>`;
        units.push({
          kind: 'question',
          qIndex: i,
          isQuestionHead: ci === 0,
          item,
          prefix: ci === 0 ? prefix : '',
          faceHtml: ci === 0 ? face : '',
          html: `<article class="${cls}">${inner}</article>`
        });
      }
    } else {
      units.push({
        kind: 'question',
        qIndex: i,
        isQuestionHead: true,
        item,
        prefix,
        faceHtml: face,
        html: `<article class="q-item"><div class="q-body"><span class="q-no">${escapeDoc(prefix)}</span><div class="doc">${face}</div></div></article>`
      });
    }

    if (i === lastChoiceIdx && choiceEntries.length) {
      units.push({
        kind: 'answer_box',
        choiceEntries,
        html: buildChoiceAnswerBoxHtml(choiceEntries)
      });
    }
  }
  return units;
}

function measureChoiceAnswerBoxInPage(doc, refPage, pageNo, choiceEntries) {
  if (!choiceEntries || !choiceEntries.length) return null;
  const pageRect = refPage.getBoundingClientRect();
  if (!pageRect.width || !pageRect.height) return buildFallbackAnswerBoxMeta(choiceEntries, pageNo);
  const cellNodes = Array.from(refPage.querySelectorAll('td.ans-write-cell[data-qno]'));
  if (cellNodes.length !== choiceEntries.length) return buildFallbackAnswerBoxMeta(choiceEntries, pageNo);

  const geom = computeChoiceBoxGridGeometry(choiceEntries.length);
  const titleCell = refPage.querySelector('th.ans-title-cell');
  const gridTbl = refPage.querySelector('#bf-choice-ans-grid');
  const titleRect = (titleCell || gridTbl)?.getBoundingClientRect();
  let minL = Infinity, minT = Infinity, maxB = -Infinity;
  const rowTops = new Map();
  const rowBottoms = new Map();
  for (const td of cellNodes) {
    const r = td.getBoundingClientRect();
    minL = Math.min(minL, r.left - pageRect.left);
    minT = Math.min(minT, r.top - pageRect.top);
    maxB = Math.max(maxB, r.bottom - pageRect.top);
  }
  const gridOriginMm = titleRect
    ? {
        x: roundMm2(((titleRect.left - pageRect.left) / pageRect.width) * 210),
        y: roundMm2(((titleRect.top - pageRect.top) / pageRect.height) * 297)
      }
    : {
        x: roundMm2((minL / pageRect.width) * 210),
        y: roundMm2(((minT / pageRect.height) * 297) - CHOICE_GRID_TITLE_H_MM - CHOICE_GRID_HEADER_H_MM)
      };

  const cells = cellNodes.map((td, idx) => {
    const r = td.getBoundingClientRect();
    const left = r.left - pageRect.left;
    const top = r.top - pageRect.top;
    const bottom = r.bottom - pageRect.top;
    const rowIdx = Number(td.getAttribute('data-row') || Math.floor(idx / geom.cols));
    const colIdx = Number(td.getAttribute('data-col') || (idx % geom.cols));
    if (!rowTops.has(rowIdx)) rowTops.set(rowIdx, top);
    if (!rowBottoms.has(rowIdx)) rowBottoms.set(rowIdx, bottom);

    const nx = left / pageRect.width;
    const ny = top / pageRect.height;
    const nw = r.width / pageRect.width;
    const nh = r.height / pageRect.height;
    const rectMm = {
      x: roundMm2(nx * 210),
      y: roundMm2(ny * 297),
      w: roundMm2(nw * 210),
      h: roundMm2(nh * 297)
    };
    const e = choiceEntries[idx];
    return {
      no: Number(td.getAttribute('data-qno') || e.no),
      qIndex: Number(td.getAttribute('data-qno') || e.no),
      qId: td.getAttribute('data-qid') || e.qId || '',
      type: td.getAttribute('data-qtype') || e.type,
      qType: td.getAttribute('data-qtype') || e.type,
      answer: e.answer || '',
      score: e.score,
      row: rowIdx,
      col: colIdx,
      rectMm,
      normRect: toAnchorNormRect(rectMm),
      x: roundCoord4(nx),
      y: roundCoord4(ny),
      w: roundCoord4(nw),
      h: roundCoord4(nh),
      bbox_mm: rectMm
    };
  });

  const gridWidthPx = gridTbl ? gridTbl.getBoundingClientRect().width : (cells[0].w * pageRect.width * geom.cols);
  const bx = roundCoord4(minL / pageRect.width);
  const by = roundCoord4(minT / pageRect.height);
  const bw = roundCoord4(gridWidthPx / pageRect.width);
  const bh = roundCoord4((maxB - minT) / pageRect.height);
  const boxRectMm = {
    x: roundMm2((minL / pageRect.width) * 210),
    y: roundMm2((minT / pageRect.height) * 297),
    w: roundMm2((gridWidthPx / pageRect.width) * 210),
    h: roundMm2(((maxB - minT) / pageRect.height) * 297)
  };
  const firstCell = cells[0];
  let rowGapNorm = 0;
  if (geom.rows > 1 && rowTops.has(1) && rowBottoms.has(0)) {
    rowGapNorm = Math.max(0, (rowTops.get(1) - rowBottoms.get(0)) / pageRect.height);
  }

  return {
    anchorToken: GRID_ANCHOR_TOKEN,
    page: pageNo,
    coord_system: 'anchor_norm_rect',
    gridOriginMm,
    columnsPerRow: geom.cols,
    rows: geom.rows,
    cols: geom.cols,
    cellWidthMm: firstCell.rectMm.w,
    titleHeightMm: CHOICE_GRID_TITLE_H_MM,
    headerHeightMm: CHOICE_GRID_HEADER_H_MM,
    writeBoxHeightMm: firstCell.rectMm.h,
    cell_w_mm: firstCell.rectMm.w,
    title_h_mm: CHOICE_GRID_TITLE_H_MM,
    header_h_mm: CHOICE_GRID_HEADER_H_MM,
    cell_h_mm: firstCell.rectMm.h,
    row_gap_mm: roundMm2(rowGapNorm * 297),
    rectMm: boxRectMm,
    normRect: toAnchorNormRect(boxRectMm),
    x: bx,
    y: by,
    w: bw,
    h: bh,
    bbox: { x: bx, y: by, w: bw, h: bh },
    bbox_mm: boxRectMm,
    cell_w: firstCell.w,
    cell_h: firstCell.h,
    col_gap: 0,
    row_gap: roundCoord4(rowGapNorm),
    question_nos: cells.map(c => c.no),
    cells
  };
}

async function paginateStudentPaper(edition, items, opts = {}) {
  const digits = normalizeStudentIdDigits(opts.studentIdDigits);
  const schoolHeader = String(opts.schoolHeader || '').trim();
  const safetyRatio = opts.safetyRatio ?? 0.99;
  const units = opts.units || await prepareStudentFlowUnits(edition, items);
  const choiceEntries = collectChoiceBoxEntries(edition, items);
  if (typeof document === 'undefined' || !document.body) {
    return {
      pageCount: null,
      pages: [{ pageNo: 1, units }],
      questionPages: null,
      answerBox: buildFallbackAnswerBoxMeta(choiceEntries, 1),
      units
    };
  }
  const leadHtml = buildStudentLeadHtml(edition, items, digits);
  const hdrHtml = schoolHeader ? `<div class="hdr-school">${escapeDoc(schoolHeader)}</div>` : '';
  const measureHtml = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<style>${GRADING_A4_PRINT_CSS}
#measure-pool { position: absolute; left: 0; top: 0; width: 180mm; visibility: hidden; }
</style></head><body>
<div class="grading-sheet" id="ref-page">
  ${hdrHtml}
  <div class="sheet-content" id="ref-body"></div>
</div>
<div id="measure-pool">
  <div id="m-lead">${leadHtml}</div>
  ${units.map((u, idx) => `<div data-u="${idx}">${u.html}</div>`).join('')}
</div>
</body></html>`;

  const frame = document.createElement('iframe');
  frame.setAttribute('aria-hidden', 'true');
  frame.style.cssText = 'position:fixed;left:-99999px;top:0;width:220mm;height:310mm;border:0;visibility:hidden;pointer-events:none';
  document.body.appendChild(frame);
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(Error('分页测量超时')), 15000);
      frame.onload = async () => {
        clearTimeout(timer);
        try {
          const imgs = Array.from(frame.contentDocument?.images || []);
          await Promise.all(imgs.map(img => img.complete ? Promise.resolve() : new Promise(r => { img.onload = r; img.onerror = r; })));
          if (frame.contentDocument?.fonts?.ready) {
            await Promise.race([frame.contentDocument.fonts.ready, new Promise(r => setTimeout(r, 1500))]);
          }
          resolve();
        } catch (e) { reject(e); }
      };
      frame.srcdoc = measureHtml;
    });
    const doc = frame.contentDocument;
    const refPage = doc.getElementById('ref-page');
    const refBody = doc.getElementById('ref-body');
    const refBodyH = refBody?.clientHeight || 0;
    if (refBodyH < 200) {
      return {
        pageCount: null,
        pages: [{ pageNo: 1, units }],
        questionPages: null,
        answerBox: buildFallbackAnswerBoxMeta(choiceEntries, 1),
        units
      };
    }
    const usable = Math.floor(refBodyH * safetyRatio);
    const leadNode = doc.getElementById('m-lead')?.firstElementChild;
    const unitNodes = units.map((_, idx) => doc.querySelector(`[data-u="${idx}"]`)?.firstElementChild);

    const pages = [];
    let current = { pageNo: 1, units: [] };
    refBody.innerHTML = '';
    if (leadNode) refBody.appendChild(leadNode.cloneNode(true));

    const startNextPage = () => {
      pages.push(current);
      current = { pageNo: pages.length + 1, units: [] };
      refBody.innerHTML = '';
    };
    const overflows = () => {
      const last = refBody.lastElementChild;
      if (!last) return false;
      const usedH = last.getBoundingClientRect().bottom - refBody.getBoundingClientRect().top;
      return usedH > usable || refBody.scrollHeight > refBodyH + 1;
    };

    for (let i = 0; i < units.length; i++) {
      const u = units[i];
      const node = unitNodes[i];
      if (!node) { current.units.push(u); continue; }

      if (u.kind === 'section' && i + 1 < units.length && unitNodes[i + 1]) {
        const hasContent = current.units.length > 0 || current.pageNo === 1;
        const hClone = node.cloneNode(true);
        const qClone = unitNodes[i + 1].cloneNode(true);
        refBody.appendChild(hClone);
        refBody.appendChild(qClone);
        const pairOver = overflows();
        refBody.removeChild(qClone);
        if (pairOver && hasContent) {
          refBody.removeChild(hClone);
          startNextPage();
          refBody.appendChild(node.cloneNode(true));
        }
        current.units.push(u);
        continue;
      }

      const clone = node.cloneNode(true);
      refBody.appendChild(clone);
      if (overflows() && (current.units.length > 0 || current.pageNo === 1)) {
        refBody.removeChild(clone);
        startNextPage();
        refBody.appendChild(node.cloneNode(true));
      }
      current.units.push(u);
    }
    pages.push(current);

    const questionPages = new Map();
    let answerBox = null;
    for (const p of pages) {
      let hasAnsBox = false;
      for (const u of p.units) {
        if (u.kind === 'question' && !questionPages.has(u.qIndex)) {
          questionPages.set(u.qIndex, p.pageNo);
        }
        if (u.kind === 'answer_box') hasAnsBox = true;
      }
      if (hasAnsBox && choiceEntries.length) {
        refBody.innerHTML = (p.pageNo === 1 ? leadHtml : '') + p.units.map(u => u.html).join('');
        answerBox = measureChoiceAnswerBoxInPage(doc, refPage, p.pageNo, choiceEntries);
      }
    }

    return { pageCount: pages.length, pages, questionPages, answerBox, units };
  } catch {
    return {
      pageCount: null,
      pages: [{ pageNo: 1, units }],
      questionPages: null,
      answerBox: buildFallbackAnswerBoxMeta(choiceEntries, 1),
      units
    };
  } finally {
    frame.remove();
  }
}

function buildGradingStampOverlayHtml(paperId, pageNo, totalPages) {
  const code = formatQrPayload(paperId, pageNo);
  const svg = qrSvgString(code, { quiet: 0, ecLevel: 'Q', sizeMm: GRADING_QR_SIZE_MM });
  return `<div class="sheet-stamp" aria-hidden="true">
    <span class="reg-mark reg-tl"></span>
    <span class="reg-mark reg-tr"></span>
    <span class="reg-mark reg-bl"></span>
    <span class="reg-mark reg-br"></span>
    <footer class="stamp-ftr">
      <span class="ftr-page-no">第 ${pageNo} 页 / 共 ${totalPages} 页</span>
      <span class="ftr-qr-wrap">
        <span class="ftr-qr-code">${escapeDoc(code)}</span>
        ${svg}
      </span>
    </footer>
  </div>`;
}

async function buildGradingPrintHtml(edition, items, opts = {}) {
  const paperId = opts.paperId || await computePaperId(edition);
  const digits = normalizeStudentIdDigits(opts.studentIdDigits);
  const schoolHeader = String(opts.schoolHeader || '').trim();
  const layout = opts.layout || await paginateStudentPaper(edition, items, {
    studentIdDigits: digits,
    schoolHeader,
    safetyRatio: 0.99
  });
  const totalPages = layout.pages.length;
  const leadHtml = buildStudentLeadHtml(edition, items, digits);
  const hdrHtml = schoolHeader ? `<div class="hdr-school">${escapeDoc(schoolHeader)}</div>` : '';

  const stampedSheets = layout.pages.map(p => {
    const bodyHtml = (p.pageNo === 1 ? leadHtml : '') + p.units.map(u => u.html).join('');
    const stampHtml = buildGradingStampOverlayHtml(paperId, p.pageNo, totalPages);
    return `<div class="grading-sheet" data-page="${p.pageNo}">
      ${hdrHtml}
      <div class="sheet-content">${bodyHtml}</div>
      ${stampHtml}
    </div>`;
  });

  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:;">
<title>${escapeDoc(edition?.name || '生物试卷')}</title>
<style>${GRADING_A4_PRINT_CSS}</style>${'</' + 'head>'}<body>${stampedSheets.join('')}</body></html>`;
}

// ---------- Vector PDF Stamper (pdf-lib drawRectangle) + BFGRID0 Anchor Extractor ----------

async function inspectPdfForSealing(pdfBytes, totalQuestions = 0) {
  if (typeof loadPdfRuntime !== 'function') throw Error('PDF 解析组件尚未就绪，请刷新后重试。');
  const { pdf, options } = await loadPdfRuntime();
  const doc = await pdf.getDocument({ ...options, data: new Uint8Array(pdfBytes.slice(0)) }).promise;
  let probe = null;
  const hasCenterPageNo = [];
  const questionPages = new Map();
  try {
    const total = doc.numPages;
    if (!total) throw Error('所选 PDF 没有页面。');
    for (let n = 1; n <= total; n++) {
      const page = await doc.getPage(n);
      const vp = page.getViewport({ scale: 1 });
      const tc = await page.getTextContent();
      let pageHasCenterFtr = false;
      for (const it of tc.items || []) {
        const str = typeof it.str === 'string' ? it.str : '';
        if (!str || !Array.isArray(it.transform)) continue;
        const tx = Number(it.transform[4]) || 0;
        const ty = Number(it.transform[5]) || 0;
        const xMm = (tx / vp.width) * 210.0;
        const yBottomMm = (ty / vp.height) * 297.0;
        const yTopMm = 297.0 - yBottomMm;

        if (!probe && str.includes(GRID_ANCHOR_TOKEN)) {
          // BFGRID0 sits at left of 5.0mm Title Row (0.35mm cell left margin)
          const gridOriginXMm = Math.max(0, xMm - 0.35);
          const gridOriginYMm = Math.max(0, yTopMm - GRID_ANCHOR_BASELINE_FROM_TOP_MM);
          probe = {
            pageNo: n,
            gridOriginXMm,
            gridOriginYMm,
            originXMm: gridOriginXMm,
            writeRow0TopMm: gridOriginYMm + CHOICE_GRID_TITLE_H_MM + CHOICE_GRID_HEADER_H_MM
          };
        }
        if (yBottomMm >= 3.0 && yBottomMm <= 21.0 && xMm >= 78.0 && xMm <= 132.0 && str.trim()) {
          pageHasCenterFtr = true;
        }
        if (totalQuestions > 0 && xMm <= 30.0) {
          const m = str.trim().match(/^(\d{1,2})\s*[．.、]/);
          if (m) {
            const qNo = Number(m[1]);
            if (qNo >= 1 && qNo <= totalQuestions && !questionPages.has(qNo - 1)) {
              questionPages.set(qNo - 1, n);
            }
          }
        }
      }
      hasCenterPageNo.push(pageHasCenterFtr);
    }
    return {
      pageCount: total,
      probe,
      hasCenterPageNo,
      questionPages: questionPages.size ? questionPages : null
    };
  } finally {
    await doc.destroy();
  }
}

async function extractAnswerGridProbeFromPdfDoc(pdfDoc) {
  for (let n = 1; n <= pdfDoc.numPages; n++) {
    const page = await pdfDoc.getPage(n);
    const vp = page.getViewport({ scale: 1 });
    const tc = await page.getTextContent();
    for (const it of tc.items || []) {
      const str = typeof it.str === 'string' ? it.str : '';
      if (str.includes(GRID_ANCHOR_TOKEN) && Array.isArray(it.transform)) {
        const tx = Number(it.transform[4]) || 0;
        const ty = Number(it.transform[5]) || 0;
        const xMm = (tx / vp.width) * 210.0;
        const yTopMm = ((vp.height - ty) / vp.height) * 297.0;
        const gridOriginXMm = Math.max(0, xMm - 0.35);
        const gridOriginYMm = Math.max(0, yTopMm - GRID_ANCHOR_BASELINE_FROM_TOP_MM);
        return {
          pageNo: n,
          gridOriginXMm,
          gridOriginYMm,
          originXMm: gridOriginXMm,
          writeRow0TopMm: gridOriginYMm + CHOICE_GRID_TITLE_H_MM + CHOICE_GRID_HEADER_H_MM
        };
      }
    }
  }
  return null;
}

// Draws 4x4mm corner anchors and 15mm vector QR codes directly with pdf-lib drawRectangle
// (zero raster PNG, zero scaling/anti-aliasing blur, preserves 100% vector PDF text & figures).
async function stampPdfVectorWithPdfLib(pdfBytes, opts = {}) {
  await loadTool('pdflib');
  const lib = typeof PDFLib !== 'undefined' ? PDFLib : globalThis.PDFLib;
  if (!lib || !lib.PDFDocument) throw Error('矢量盖章组件（pdf-lib）尚未加载，请刷新页面重试。');
  const paperId = String(opts.paperId || '').trim();
  if (!paperId) throw Error('缺少试卷 ID，无法执行矢量盖章。');

  const { PDFDocument, rgb, StandardFonts } = lib;
  const pdfDoc = await PDFDocument.load(pdfBytes);
  const pages = pdfDoc.getPages();
  const totalPages = pages.length;
  if (!totalPages) throw Error('PDF 文件不包含任何页面。');

  const courierFont = await pdfDoc.embedFont(StandardFonts.Courier);
  const helvFont = await pdfDoc.embedFont(StandardFonts.Helvetica);
  const black = rgb(0, 0, 0);
  const white = rgb(1, 1, 1);
  const codeColor = rgb(0.08, 0.08, 0.08);
  const pgColor = rgb(0.2, 0.2, 0.2);
  const hasCenterPageNo = Array.isArray(opts.hasCenterPageNo) ? opts.hasCenterPageNo : [];

  for (let i = 0; i < totalPages; i++) {
    const page = pages[i];
    const pageNo = i + 1;
    const { width: wPt, height: hPt } = page.getSize();
    const mmToPt = wPt / 210.0;

    // 1. Mask Word draft warning on left & placeholder on right while preserving Word's native CJK center page number
    if (hasCenterPageNo[i]) {
      page.drawRectangle({
        x: 11.0 * mmToPt,
        y: 3.8 * mmToPt,
        width: 71.0 * mmToPt,
        height: 17.2 * mmToPt,
        color: white,
        borderWidth: 0
      });
      page.drawRectangle({
        x: 127.0 * mmToPt,
        y: 3.8 * mmToPt,
        width: 71.5 * mmToPt,
        height: 17.2 * mmToPt,
        color: white,
        borderWidth: 0
      });
    } else {
      page.drawRectangle({
        x: 11.0 * mmToPt,
        y: 3.8 * mmToPt,
        width: 187.5 * mmToPt,
        height: 17.2 * mmToPt,
        color: white,
        borderWidth: 0
      });
      const pgLabel = `${pageNo} / ${totalPages}`;
      const pgSize = 8.5;
      const pgW = helvFont.widthOfTextAtSize(pgLabel, pgSize);
      page.drawText(pgLabel, {
        x: (wPt - pgW) * 0.5,
        y: 9.2 * mmToPt,
        size: pgSize,
        font: helvFont,
        color: pgColor
      });
    }

    // 2. Four 4mm x 4mm solid black corner registration fiducials (6mm inset -> 194x281mm ref frame)
    const insetPt = GRADING_ANCHOR_INSET_MM * mmToPt;
    const anchorPt = GRADING_ANCHOR_SIZE_MM * mmToPt;
    page.drawRectangle({ x: insetPt, y: hPt - insetPt - anchorPt, width: anchorPt, height: anchorPt, color: black, borderWidth: 0 });
    page.drawRectangle({ x: wPt - insetPt - anchorPt, y: hPt - insetPt - anchorPt, width: anchorPt, height: anchorPt, color: black, borderWidth: 0 });
    page.drawRectangle({ x: insetPt, y: insetPt, width: anchorPt, height: anchorPt, color: black, borderWidth: 0 });
    page.drawRectangle({ x: wPt - insetPt - anchorPt, y: insetPt, width: anchorPt, height: anchorPt, color: black, borderWidth: 0 });

    // 3. Vector 15mm x 15mm footer QR at x = 180mm, bottom = 5mm (pure drawRectangle black runs, zero anti-aliasing)
    const code = formatQrPayload(paperId, pageNo);
    const { size, modules } = qrMatrix(code, 'Q');
    const qrPt = GRADING_QR_SIZE_MM * mmToPt;
    const qrX = GRADING_FOOTER_QR_X_MM * mmToPt;
    const qrY = GRADING_FOOTER_QR_BOTTOM_MM * mmToPt;
    const modPt = qrPt / size;

    page.drawRectangle({
      x: qrX - 1.5 * mmToPt,
      y: qrY - 1.2 * mmToPt,
      width: qrPt + 3.0 * mmToPt,
      height: qrPt + 2.4 * mmToPt,
      color: white,
      borderWidth: 0
    });

    for (let r = 0; r < size; r++) {
      let c = 0;
      while (c < size) {
        if (!modules[r][c]) { c++; continue; }
        let run = 1;
        while (c + run < size && modules[r][c + run]) run++;
        page.drawRectangle({
          x: qrX + c * modPt,
          y: qrY + (size - 1 - r) * modPt,
          width: run * modPt,
          height: modPt,
          color: black,
          borderWidth: 0
        });
        c += run;
      }
    }

    // 4. Single-line payload label to the left of QR
    const codeFontSize = 7.0;
    const codeW = courierFont.widthOfTextAtSize(code, codeFontSize);
    page.drawText(code, {
      x: qrX - 3.0 * mmToPt - codeW,
      y: qrY + 1.2 * mmToPt,
      size: codeFontSize,
      font: courierFont,
      color: codeColor
    });
  }

  return pdfDoc.save();
}

// Full sealing pipeline: inspects PDF for BFGRID0 (blocking if missing when objective items exist!),
// stamps vector 15mm QR + 4mm corner anchors via pdf-lib, and rebuilds schemaVersion 2.1 JSON.
async function sealPdfAndBuildGradingPack(pdfBytes, edition = null, items = null, opts = {}) {
  const paperId = String(opts.paperId || (edition ? await computePaperId(edition) : '')).trim();
  if (!paperId) throw Error('缺少试卷 ID，无法盖章封版。');
  const choiceEntries = (edition && Array.isArray(items)) ? collectChoiceBoxEntries(edition, items) : [];
  const inspected = await inspectPdfForSealing(pdfBytes, Array.isArray(items) ? items.length : 0);

  if ((choiceEntries.length > 0 || opts.requireGridAnchor) && !inspected.probe) {
    const err = new Error('未在文档中检测到客观题答题框定位锚点（BFGRID0）。可能客观题答题框标题行被误删或表格结构被破坏；为防止整批答卷切图错位，已阻断盖章与批改包生成。请恢复客观题答题框或重新导出学生版 Word 后再试。');
    err.code = 'BFGRID0_MISSING';
    throw err;
  }

  const stampedPdfBytes = await stampPdfVectorWithPdfLib(pdfBytes, {
    paperId,
    hasCenterPageNo: inspected.hasCenterPageNo
  });

  let pack = null;
  if (edition && Array.isArray(items)) {
    const answerBox = (choiceEntries.length && inspected.probe)
      ? buildAnswerBoxFromGridOriginMm(
          choiceEntries,
          inspected.probe.pageNo,
          inspected.probe.gridOriginXMm,
          inspected.probe.gridOriginYMm
        )
      : null;
    pack = buildGradingPack(edition, items, {
      paperId,
      studentIdDigits: opts.studentIdDigits,
      pageCount: inspected.pageCount,
      questionPages: inspected.questionPages,
      answerBox
    });
  }

  return {
    stampedPdfBytes,
    pack,
    pageCount: inspected.pageCount,
    probeFound: Boolean(inspected.probe),
    probe: inspected.probe
  };
}

async function stampExternalPdfAndRebuildPack(pdfBytes, edition = null, items = null, opts = {}) {
  return sealPdfAndBuildGradingPack(pdfBytes, edition, items, opts);
}

// ---------- Grading Word (.docx): Single-Section Continuous + 180mm 10-Col Grid ----------

function makeStudentIdBoxWordBlocks(digitsRaw) {
  const digits = normalizeStudentIdDigits(digitsRaw);
  const D = docx;
  const noBorder = { style: D.BorderStyle.NONE, size: 0, color: 'auto' };
  const boxBorder = { style: D.BorderStyle.SINGLE, size: 6, color: '000000' };
  const noneBorders = { top: noBorder, bottom: noBorder, left: noBorder, right: noBorder };
  const allBoxBorders = { top: boxBorder, bottom: boxBorder, left: boxBorder, right: boxBorder };
  const zeroMargins = { top: 0, bottom: 0, left: 0, right: 0 };

  const makeBoxCells = () => {
    const cells = [];
    for (let i = 0; i < digits; i++) {
      if (i > 0) {
        cells.push(new D.TableCell({
          width: { size: 102, type: D.WidthType.DXA },
          borders: noneBorders,
          margins: zeroMargins,
          children: [new D.Paragraph({ spacing: { before: 0, after: 0 }, children: [] })]
        }));
      }
      cells.push(new D.TableCell({
        width: { size: 340, type: D.WidthType.DXA },
        borders: allBoxBorders,
        margins: zeroMargins,
        verticalAlign: D.VerticalAlign.CENTER,
        children: [new D.Paragraph({ spacing: { before: 0, after: 0 }, children: [] })]
      }));
    }
    return cells;
  };

  const makeBoxColumnWidths = () => {
    const widths = [];
    for (let i = 0; i < digits; i++) {
      if (i > 0) widths.push(102);
      widths.push(340);
    }
    return widths;
  };

  if (digits <= 8) {
    const labelW = 5000;
    const colWidths = [labelW, ...makeBoxColumnWidths()];
    const totalW = colWidths.reduce((a, b) => a + b, 0);
    const labelCell = new D.TableCell({
      width: { size: labelW, type: D.WidthType.DXA },
      borders: noneBorders,
      margins: { top: 0, bottom: 0, left: 0, right: 60 },
      verticalAlign: D.VerticalAlign.CENTER,
      children: [new D.Paragraph({
        spacing: { before: 0, after: 0 },
        children: [new D.TextRun({ text: '班级：__________　姓名：__________　学号：', size: 21, font: WORD_BODY_FONT })]
      })]
    });
    return [
      new D.Table({
        alignment: D.AlignmentType.CENTER,
        layout: D.TableLayoutType.FIXED,
        width: { size: totalW, type: D.WidthType.DXA },
        columnWidths: colWidths,
        borders: { top: noBorder, bottom: noBorder, left: noBorder, right: noBorder, insideHorizontal: noBorder, insideVertical: noBorder },
        rows: [new D.TableRow({
          height: { value: 340, rule: D.HeightRule.EXACT },
          children: [labelCell, ...makeBoxCells()]
        })]
      }),
      new D.Paragraph({ snapToGrid: false, spacing: { before: 0, after: 40, line: 60, lineRule: D.LineRuleType.EXACT }, children: [] })
    ];
  }

  const idLabelW = 820;
  const colWidths = [idLabelW, ...makeBoxColumnWidths()];
  const totalW = colWidths.reduce((a, b) => a + b, 0);
  const idLabelCell = new D.TableCell({
    width: { size: idLabelW, type: D.WidthType.DXA },
    borders: noneBorders,
    margins: { top: 0, bottom: 0, left: 0, right: 60 },
    verticalAlign: D.VerticalAlign.CENTER,
    children: [new D.Paragraph({
      spacing: { before: 0, after: 0 },
      children: [new D.TextRun({ text: '学号：', size: 21, font: WORD_BODY_FONT })]
    })]
  });
  return [
    new D.Paragraph({
      alignment: D.AlignmentType.CENTER,
      spacing: { before: 0, after: 60 },
      children: [new D.TextRun({ text: '班级：______________　　姓名：______________', size: 21, font: WORD_BODY_FONT })]
    }),
    new D.Table({
      alignment: D.AlignmentType.CENTER,
      layout: D.TableLayoutType.FIXED,
      width: { size: totalW, type: D.WidthType.DXA },
      columnWidths: colWidths,
      borders: { top: noBorder, bottom: noBorder, left: noBorder, right: noBorder, insideHorizontal: noBorder, insideVertical: noBorder },
      rows: [new D.TableRow({
        height: { value: 340, rule: D.HeightRule.EXACT },
        children: [idLabelCell, ...makeBoxCells()]
      })]
    }),
    new D.Paragraph({ spacing: { before: 0, after: 80 }, children: [] })
  ];
}

function makeChoiceAnswerBoxWordBlocks(entries) {
  if (!entries || !entries.length) return [];
  const D = docx;
  const geom = computeChoiceBoxGridGeometry(entries.length);
  const tip = buildChoiceBoxInstruction(entries);
  // 10 columns x 18.0mm = 180.0mm = 10206 dxa exact ([1021*6, 1020*4])
  const colWidths = [1021, 1021, 1021, 1021, 1021, 1021, 1020, 1020, 1020, 1020];
  const totalW = 10206;
  const titleHDxa = 283;  // 5.00 mm EXACT
  const labelHDxa = 283;  // 5.00 mm EXACT
  const writeHDxa = 567;  // 10.00 mm EXACT

  const noBorder = { style: D.BorderStyle.NONE, size: 0, color: 'auto' };
  const border075 = { style: D.BorderStyle.SINGLE, size: 6, color: '000000' }; // 0.75pt solid #000000
  const border100 = { style: D.BorderStyle.SINGLE, size: 8, color: '000000' }; // 1.00pt solid #000000
  const zeroMargins = { top: 0, bottom: 0, left: 0, right: 0 };

  const tableRows = [
    new D.TableRow({
      cantSplit: true,
      height: { value: titleHDxa, rule: D.HeightRule.EXACT },
      children: [
        new D.TableCell({
          columnSpan: geom.cols,
          width: { size: totalW, type: D.WidthType.DXA },
          borders: { top: border075, bottom: border075, left: border075, right: border075 },
          margins: { top: 0, bottom: 0, left: 20, right: 40 },
          shading: { fill: 'FAFAFA' },
          verticalAlign: D.VerticalAlign.CENTER,
          children: [new D.Paragraph({
            alignment: D.AlignmentType.LEFT,
            keepNext: true,
            keepLines: true,
            snapToGrid: false,
            spacing: { before: 0, after: 0, line: 220 },
            children: [
              new D.TextRun({ text: GRID_ANCHOR_TOKEN, size: 4, color: 'EFEFEF', font: 'Consolas' }),
              new D.TextRun({ text: ' ' + tip, bold: true, size: 17, color: '111111', font: WORD_BODY_FONT })
            ]
          })]
        })
      ]
    })
  ];

  for (let r = 0; r < geom.rows; r++) {
    const isLastRowGroup = r === geom.rows - 1;
    const numCells = [];
    const writeCells = [];
    for (let c = 0; c < geom.cols; c++) {
      const idx = r * geom.cols + c;
      const cellW = colWidths[c];
      const entry = idx < entries.length ? entries[idx] : null;
      const keepWriteNext = !isLastRowGroup;

      if (entry) {
        numCells.push(new D.TableCell({
          width: { size: cellW, type: D.WidthType.DXA },
          borders: { top: r === 0 ? border075 : border100, bottom: border100, left: border075, right: border075 },
          margins: zeroMargins,
          shading: { fill: 'F2F2F2' },
          verticalAlign: D.VerticalAlign.CENTER,
          children: [new D.Paragraph({
            alignment: D.AlignmentType.CENTER,
            keepNext: true,
            keepLines: true,
            snapToGrid: false,
            spacing: { before: 0, after: 0, line: 200 },
            children: [new D.TextRun({
              text: String(entry.no),
              bold: true,
              size: 18,
              color: '111111',
              font: 'Times New Roman'
            })]
          })]
        }));

        writeCells.push(new D.TableCell({
          width: { size: cellW, type: D.WidthType.DXA },
          borders: { top: border100, bottom: border100, left: border100, right: border100 },
          margins: zeroMargins,
          verticalAlign: D.VerticalAlign.CENTER,
          children: [new D.Paragraph({
            alignment: D.AlignmentType.CENTER,
            ...(keepWriteNext ? { keepNext: true } : {}),
            keepLines: true,
            snapToGrid: false,
            spacing: { before: 0, after: 0, line: 200 },
            children: []
          })]
        }));
      } else {
        const isAfterLast = idx === entries.length;
        numCells.push(new D.TableCell({
          width: { size: cellW, type: D.WidthType.DXA },
          borders: {
            top: r === 0 ? border075 : border100,
            bottom: noBorder,
            left: isAfterLast ? border075 : noBorder,
            right: noBorder
          },
          margins: zeroMargins,
          verticalAlign: D.VerticalAlign.CENTER,
          children: [new D.Paragraph({
            keepNext: true,
            keepLines: true,
            snapToGrid: false,
            spacing: { before: 0, after: 0, line: 200 },
            children: []
          })]
        }));

        writeCells.push(new D.TableCell({
          width: { size: cellW, type: D.WidthType.DXA },
          borders: {
            top: noBorder,
            bottom: noBorder,
            left: isAfterLast ? border100 : noBorder,
            right: noBorder
          },
          margins: zeroMargins,
          verticalAlign: D.VerticalAlign.CENTER,
          children: [new D.Paragraph({
            ...(keepWriteNext ? { keepNext: true } : {}),
            keepLines: true,
            snapToGrid: false,
            spacing: { before: 0, after: 0, line: 200 },
            children: []
          })]
        }));
      }
    }
    tableRows.push(new D.TableRow({
      cantSplit: true,
      height: { value: labelHDxa, rule: D.HeightRule.EXACT },
      children: numCells
    }));
    tableRows.push(new D.TableRow({
      cantSplit: true,
      height: { value: writeHDxa, rule: D.HeightRule.EXACT },
      children: writeCells
    }));
  }

  return [
    new D.Paragraph({ snapToGrid: false, keepNext: true, spacing: { before: 0, after: 40, line: 80, lineRule: D.LineRuleType.EXACT }, children: [] }),
    new D.Table({
      layout: D.TableLayoutType.FIXED,
      width: { size: totalW, type: D.WidthType.DXA },
      columnWidths: colWidths,
      borders: { top: noBorder, bottom: noBorder, left: noBorder, right: noBorder, insideHorizontal: noBorder, insideVertical: noBorder },
      rows: tableRows
    }),
    new D.Paragraph({ snapToGrid: false, spacing: { before: 0, after: 60, line: 120, lineRule: D.LineRuleType.EXACT }, children: [] })
  ];
}

async function makeGradingBaseTemplate(schoolHeader = '', studentMode = true) {
  await loadTool('docx');
  const D = docx, C = D.AlignmentType.CENTER;
  const line = (text, opts = {}) => new D.Paragraph({
    alignment: opts.align,
    snapToGrid: false,
    spacing: { after: opts.after ?? 40, line: 280 },
    children: [new D.TextRun({ text, bold: opts.bold, size: opts.size, font: opts.font || WORD_BODY_FONT })]
  });
  const hdrText = String(schoolHeader || '').trim();
  const children = [
    line('{{试卷名称}}', { align: C, bold: true, size: studentMode ? 28 : 30, after: studentMode ? 36 : 60 }),
    ...(studentMode ? [line('{{学号方框}}', { align: C, after: 36 })] : []),
    line('{{试题}}', { after: 0 })
  ];
  return D.Packer.toBlob(new D.Document({
    styles: {
      default: {
        document: {
          run: { font: WORD_BODY_FONT, size: 24 },
          paragraph: studentMode ? { spacing: { line: 268, after: 20 }, snapToGrid: false } : {}
        }
      }
    },
    sections: [{
      properties: {
        page: {
          size: { width: 11906, height: 16838 },
          margin: {
            top: hdrText ? 907 : 794,          // 16mm with school header, 14mm without
            bottom: studentMode ? 1247 : 1080, // 22mm bottom margin for 15mm footer QR
            left: 850,                         // 15mm left
            right: 850,                        // 15mm right -> 180mm (10206 dxa) body width
            header: 397,
            footer: 283
          }
        }
      },
      headers: hdrText ? { default: new D.Header({ children: [line(hdrText, { align: C, size: 18, after: 0 })] }) } : {},
      footers: {
        default: new D.Footer({
          children: [new D.Paragraph({
            alignment: C,
            children: [new D.TextRun({ children: ['第 ', D.PageNumber.CURRENT, ' 页 / 共 ', D.PageNumber.TOTAL_PAGES, ' 页'], size: 17, font: WORD_BODY_FONT })]
          })]
        })
      },
      children
    }]
  }));
}

async function makeGradingSampleTemplate(schoolHeader = '') {
  await loadTool('docx');
  const D = docx, C = D.AlignmentType.CENTER;
  const line = (text, opts = {}) => new D.Paragraph({
    alignment: opts.align,
    spacing: { after: opts.after ?? 60 },
    children: [new D.TextRun({ text, bold: opts.bold, size: opts.size, font: opts.font || WORD_BODY_FONT })]
  });
  const hdrText = String(schoolHeader || '').trim() || '××学校 2026—2027 学年第一学期';
  return D.Packer.toBlob(new D.Document({
    styles: { default: { document: { run: { font: WORD_BODY_FONT, size: 24 } } } },
    sections: [{
      properties: {
        page: {
          size: { width: 11906, height: 16838 },
          margin: { top: 907, bottom: 1247, left: 850, right: 850, header: 397, footer: 283 }
        }
      },
      headers: { default: new D.Header({ children: [line(hdrText, { align: C, size: 18, after: 0 })] }) },
      footers: {
        default: new D.Footer({
          children: [new D.Paragraph({
            alignment: C,
            children: [new D.TextRun({ children: ['第 ', D.PageNumber.CURRENT, ' 页 / 共 ', D.PageNumber.TOTAL_PAGES, ' 页'], size: 17, font: WORD_BODY_FONT })]
          })]
        })
      },
      children: [
        line('{{试卷名称}}', { align: C, bold: true, size: 30, after: 60 }),
        line('{{学号方框}}', { align: C, after: 60 }),
        line('注意事项：学号请逐位填写在上方方框内；选择题答案请工整填写在选择题末尾的答题栏内（扫描请用 300 DPI）。', { size: 18, after: 100 }),
        line('{{试题}}', { after: 0 })
      ]
    }]
  }));
}

function escapeXmlText(str) {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;');
}

function buildCornerAnchorRunXml(idBase, name, xEmu, yEmu, relId) {
  const size = 144000; // 4 mm in EMU (4 * 36000)
  return `<w:r><w:drawing><wp:anchor distT="0" distB="0" distL="0" distR="0" simplePos="0" relativeHeight="${251658240 + idBase}" behindDoc="0" locked="0" layoutInCell="0" allowOverlap="1"><wp:simplePos x="0" y="0"/><wp:positionH relativeFrom="page"><wp:posOffset>${Math.round(xEmu)}</wp:posOffset></wp:positionH><wp:positionV relativeFrom="page"><wp:posOffset>${Math.round(yEmu)}</wp:posOffset></wp:positionV><wp:extent cx="${size}" cy="${size}"/><wp:effectExtent l="0" t="0" r="0" b="0"/><wp:wrapNone/><wp:docPr id="${idBase}" name="${escapeXmlText(name)}"/><wp:cNvGraphicFramePr><a:graphicFrameLocks noChangeAspect="1"/></wp:cNvGraphicFramePr><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="${idBase + 500}" name="bf_anchor.png"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="${relId}"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="${size}" cy="${size}"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:anchor></w:drawing></w:r>`;
}

function buildSingleSectionHeaderXml(schoolHeader, pageSizeEmu = null) {
  const pageW = pageSizeEmu?.w || 7560310;
  const pageH = pageSizeEmu?.h || 10692130;
  const inset = 216000; // 6 mm inset from page edges
  const anchorSize = 144000; // 4 mm solid square
  const anchorsXml = [
    buildCornerAnchorRunXml(3011, 'BFAnchorTL', inset, inset, 'rIdAnchor'),
    buildCornerAnchorRunXml(3012, 'BFAnchorTR', pageW - inset - anchorSize, inset, 'rIdAnchor'),
    buildCornerAnchorRunXml(3013, 'BFAnchorBL', inset, pageH - inset - anchorSize, 'rIdAnchor'),
    buildCornerAnchorRunXml(3014, 'BFAnchorBR', pageW - inset - anchorSize, pageH - inset - anchorSize, 'rIdAnchor')
  ].join('');
  const schoolLines = String(schoolHeader || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  const hdrParas = schoolLines.length
    ? schoolLines.map((line, i) => `<w:p><w:pPr><w:jc w:val="center"/>${i === schoolLines.length - 1 ? '<w:pBdr><w:bottom w:val="single" w:sz="4" w:space="2" w:color="999999"/></w:pBdr>' : ''}<w:spacing w:before="0" w:after="20"/></w:pPr><w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:color w:val="333333"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr><w:t xml:space="preserve">${escapeXmlText(line)}</w:t></w:r></w:p>`).join('')
    : '';

  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">
  <w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="20" w:lineRule="exact"/></w:pPr>${anchorsXml}</w:p>
  ${hdrParas}
</w:hdr>`;
}

function buildSingleSectionFooterXml(paperId) {
  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:p>
    <w:pPr>
      <w:tabs><w:tab w:val="center" w:pos="5103"/><w:tab w:val="right" w:pos="10206"/></w:tabs>
      <w:spacing w:before="0" w:after="0"/>
    </w:pPr>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="14"/><w:szCs w:val="14"/><w:color w:val="999999"/></w:rPr><w:t xml:space="preserve">【未盖批改码草稿，请拖回网页封版后再印】&#9;</w:t></w:r>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="17"/><w:szCs w:val="17"/><w:color w:val="333333"/></w:rPr><w:t xml:space="preserve">第 </w:t></w:r>
    <w:fldSimple w:instr=" PAGE "><w:r><w:rPr><w:sz w:val="17"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="17"/><w:szCs w:val="17"/><w:color w:val="333333"/></w:rPr><w:t xml:space="preserve"> 页 / 共 </w:t></w:r>
    <w:fldSimple w:instr=" NUMPAGES "><w:r><w:rPr><w:sz w:val="17"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="17"/><w:szCs w:val="17"/><w:color w:val="333333"/></w:rPr><w:t xml:space="preserve"> 页&#9;</w:t></w:r>
    <w:r><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:eastAsia="Consolas" w:cs="Courier New"/><w:sz w:val="14"/><w:szCs w:val="14"/><w:color w:val="222222"/></w:rPr><w:t xml:space="preserve">BF:${escapeXmlText(paperId)}:</w:t></w:r>
    <w:fldSimple w:instr=" PAGE "><w:r><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas"/><w:sz w:val="14"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>
  </w:p>
</w:ftr>`;
}

async function extractTemplateHeaderText(templateBlob) {
  if (!templateBlob) return '';
  try {
    await loadTool('zip');
    const zip = await JSZip.loadAsync(await templateBlob.arrayBuffer());
    const ns = 'http://schemas.openxmlformats.org/wordprocessingml/2006/main';
    const hdrFile = Object.keys(zip.files).filter(name => /^word\/header\d*\.xml$/i.test(name)).sort()[0];
    if (!hdrFile) return '';
    const xml = new DOMParser().parseFromString(await zip.file(hdrFile).async('string'), 'application/xml');
    const paras = Array.from(xml.getElementsByTagNameNS(ns, 'p'), p =>
      Array.from(xml.getElementsByTagNameNS(ns, 't'), t => t.textContent).join('').trim()
    ).filter(Boolean);
    return paras.join('\n');
  } catch {
    return '';
  }
}

async function configureSingleSectionGradingWord(docxBlob, opts) {
  await loadTool('zip');
  const zip = await JSZip.loadAsync(await docxBlob.arrayBuffer());
  const wNs = 'http://schemas.openxmlformats.org/wordprocessingml/2006/main';
  const rNs = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships';
  const relNs = 'http://schemas.openxmlformats.org/package/2006/relationships';
  const ctNs = 'http://schemas.openxmlformats.org/package/2006/content-types';

  const docXml = new DOMParser().parseFromString(await zip.file('word/document.xml').async('string'), 'application/xml');
  if (!docXml.documentElement.getAttribute('xmlns:r')) {
    docXml.documentElement.setAttribute('xmlns:r', rNs);
  }
  const body = docXml.getElementsByTagNameNS(wNs, 'body')[0];
  if (!body) throw Error('无法写入批改版版式，Word 正文结构异常。');

  for (const p of Array.from(body.getElementsByTagNameNS(wNs, 'p'))) {
    for (const sp of Array.from(p.getElementsByTagNameNS(wNs, 'sectPr'))) {
      sp.parentNode?.removeChild(sp);
    }
  }

  const relsPath = 'word/_rels/document.xml.rels';
  const relsXml = new DOMParser().parseFromString(
    zip.file(relsPath) ? await zip.file(relsPath).async('string') : `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="${relNs}"/>`,
    'application/xml'
  );
  const ctXml = new DOMParser().parseFromString(await zip.file('[Content_Types].xml').async('string'), 'application/xml');

  let finalSectPr = Array.from(body.children).find(n => n.localName === 'sectPr');
  if (!finalSectPr) {
    finalSectPr = docXml.createElementNS(wNs, 'w:sectPr');
    body.appendChild(finalSectPr);
  }

  const schoolHeader = String(opts.schoolHeader || '').trim();
  const minTopDxa = schoolHeader ? 907 : 794; // 16mm with school header, 14mm without
  const minBottomDxa = 1247; // 22mm reserved for 15mm footer QR stamp

  let pgMar = Array.from(finalSectPr.children).find(n => n.localName === 'pgMar');
  if (!pgMar) {
    pgMar = docXml.createElementNS(wNs, 'w:pgMar');
    finalSectPr.appendChild(pgMar);
  }
  pgMar.setAttributeNS(wNs, 'w:top', String(minTopDxa));
  pgMar.setAttributeNS(wNs, 'w:bottom', String(minBottomDxa));
  pgMar.setAttributeNS(wNs, 'w:left', '850');  // 15mm
  pgMar.setAttributeNS(wNs, 'w:right', '850'); // 15mm -> 180mm (10206 dxa) body width
  pgMar.setAttributeNS(wNs, 'w:header', '397');
  pgMar.setAttributeNS(wNs, 'w:footer', '283');

  for (const dg of Array.from(finalSectPr.children).filter(n => n.localName === 'docGrid')) {
    finalSectPr.removeChild(dg);
  }
  const docGrid = docXml.createElementNS(wNs, 'w:docGrid');
  docGrid.setAttributeNS(wNs, 'w:type', 'default');
  finalSectPr.appendChild(docGrid);

  const pgSz = Array.from(finalSectPr.children).find(n => n.localName === 'pgSz');
  const pgW = Number(pgSz?.getAttributeNS(wNs, 'w') || pgSz?.getAttribute('w:w') || 11906);
  const pgH = Number(pgSz?.getAttributeNS(wNs, 'h') || pgSz?.getAttribute('w:h') || 16838);
  const pageSizeEmu = { w: pgW * 635, h: pgH * 635 };

  const hasPngType = Array.from(ctXml.getElementsByTagNameNS(ctNs, 'Default'))
    .some(el => String(el.getAttribute('Extension') || '').toLowerCase() === 'png');
  if (!hasPngType) {
    const def = ctXml.createElementNS(ctNs, 'Default');
    def.setAttribute('Extension', 'png');
    def.setAttribute('ContentType', 'image/png');
    ctXml.documentElement.appendChild(def);
  }

  zip.file('word/media/bf_anchor.png', solidBlackSquarePngBytes(32));
  zip.file('word/header_bf.xml', buildSingleSectionHeaderXml(schoolHeader, pageSizeEmu));
  zip.file('word/_rels/header_bf.xml.rels', `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="${relNs}">
  <Relationship Id="rIdAnchor" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/bf_anchor.png"/>
</Relationships>`);
  zip.file('word/footer_bf.xml', buildSingleSectionFooterXml(opts.paperId || ''));

  const addDocRelAndOverride = (id, type, target, partName, contentType) => {
    for (const r of Array.from(relsXml.getElementsByTagNameNS(relNs, 'Relationship'))) {
      if (r.getAttribute('Id') === id) r.parentNode?.removeChild(r);
    }
    const rel = relsXml.createElementNS(relNs, 'Relationship');
    rel.setAttribute('Id', id);
    rel.setAttribute('Type', type);
    rel.setAttribute('Target', target);
    relsXml.documentElement.appendChild(rel);

    for (const ov of Array.from(ctXml.getElementsByTagNameNS(ctNs, 'Override'))) {
      if (ov.getAttribute('PartName') === partName) ov.parentNode?.removeChild(ov);
    }
    const ov = ctXml.createElementNS(ctNs, 'Override');
    ov.setAttribute('PartName', partName);
    ov.setAttribute('ContentType', contentType);
    ctXml.documentElement.appendChild(ov);
  };

  addDocRelAndOverride(
    'rIdBfHdrSingle',
    'http://schemas.openxmlformats.org/officeDocument/2006/relationships/header',
    'header_bf.xml',
    '/word/header_bf.xml',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml'
  );
  addDocRelAndOverride(
    'rIdBfFtrSingle',
    'http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer',
    'footer_bf.xml',
    '/word/footer_bf.xml',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml'
  );

  for (const child of Array.from(finalSectPr.children)) {
    if (['headerReference', 'footerReference', 'titlePg', 'type'].includes(child.localName)) {
      finalSectPr.removeChild(child);
    }
  }
  const anchor = finalSectPr.firstChild;
  const hdrRef = docXml.createElementNS(wNs, 'w:headerReference');
  hdrRef.setAttributeNS(wNs, 'w:type', 'default');
  hdrRef.setAttributeNS(rNs, 'r:id', 'rIdBfHdrSingle');
  finalSectPr.insertBefore(hdrRef, anchor);

  const ftrRef = docXml.createElementNS(wNs, 'w:footerReference');
  ftrRef.setAttributeNS(wNs, 'w:type', 'default');
  ftrRef.setAttributeNS(rNs, 'r:id', 'rIdBfFtrSingle');
  finalSectPr.insertBefore(ftrRef, anchor);

  if (zip.file('word/settings.xml')) {
    const setXml = new DOMParser().parseFromString(await zip.file('word/settings.xml').async('string'), 'application/xml');
    for (const el of Array.from(setXml.getElementsByTagNameNS(wNs, 'evenAndOddHeaders'))) {
      el.parentNode?.removeChild(el);
    }
    zip.file('word/settings.xml', new XMLSerializer().serializeToString(setXml));
  }

  zip.file('word/document.xml', new XMLSerializer().serializeToString(docXml));
  zip.file(relsPath, new XMLSerializer().serializeToString(relsXml));
  zip.file('[Content_Types].xml', new XMLSerializer().serializeToString(ctXml));

  return zip.generateAsync({
    type: typeof Blob !== 'undefined' ? 'blob' : 'uint8array',
    compression: 'DEFLATE',
    mimeType: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
  });
}
