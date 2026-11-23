// Grading pack JSON export, deterministic paper_id, browser A4 pagination,
// grading-edition Word (.docx) multi-section QR stamping, and grading A4 PDF rendering.
const BUILTIN_GRADING_FORMAT_ID = '__grading__';
const DEFAULT_STUDENT_ID_DIGITS = 4;
const GRADING_PAGE_BREAK_PREFIX = '__BF_GRADING_PAGE_BREAK_';

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

// Fingerprint only immutable substantive paper structure (question order, revisions,
// per-question scores/parts, and drawing flags). Renaming title/purpose or importing
// stats keeps paper_id unchanged; changing questions or scores produces a new ID.
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

// Align per-line grading criteria with parts[]. Never fabricate splits when ambiguous.
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
      // Continuation of previous numbered blank.
      const last = buckets.findLastIndex(b => b.length > 0);
      if (last >= 0) buckets[last].push(line);
    }
  }
  if (matched >= count && buckets.every(b => b.length > 0)) {
    return { aligned: true, byIndex: buckets.map(b => b.join('；')) };
  }
  return { aligned: false, byIndex: [] };
}

// Conservatively extract full-credit accepted expressions from one blank's rule_text.
// Never extracts zero-score or partial-only phrases; returns [] when open-ended.
function extractAcceptedAnswers(ruleText, blankScore = null) {
  const raw = String(ruleText || '').trim();
  if (!raw) return [];
  // Strip trailing "(N 分)" tag appended by #grading AI drafting and leading "第X空：".
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
    // Sub-split comma-separated clauses first so a trailing ", 答错给0分" or ", 只答X给1分"
    // only drops that negative/partial sub-clause, not the full-credit clause before it.
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

    // Extract explicitly quoted terms ("...", “...”, 「...」, 『...』).
    const quoteRe = /[“「『"]([^”」』"]+)[”」』"]/g;
    let qm;
    let quotedInClause = 0;
    while ((qm = quoteRe.exec(positiveText)) !== null) {
      add(qm[1]);
      quotedInClause++;
    }
    if (quotedInClause > 0) continue;

    // Fallback only for very clean concise phrasing like: 答出叶绿体或叶绿体基质给1分.
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

function buildGradingPack(edition, items, opts = {}) {
  const grading = edition?.grading || null;
  const studentIdDigits = normalizeStudentIdDigits(opts.studentIdDigits);
  const paperId = opts.paperId || '';
  const questionPages = opts.questionPages || null;
  const pageCount = Number.isInteger(opts.pageCount) && opts.pageCount > 0 ? opts.pageCount : null;
  const generatedAt = opts.generatedAt || isoLocalTimestamp();

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
        grading_rule: rule ? markScheme(grading, q.id) : defaultObjectiveRule(kind, points, partial)
      };
      if (kind === 2) entry.partial_score = partial;
      return entry;
    }

    // Non-choice question: either drawing or blank.
    let parts = Array.isArray(rule?.parts) && rule.parts.length ? rule.parts.slice() : null;
    if (!parts && typeof initialBlankScores === 'function' && typeof parseBlankScores === 'function') {
      const fallback = parseBlankScores(initialBlankScores(q));
      parts = fallback && fallback.length ? fallback : [1];
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
    const blanks = parts.map((blankScore, i) => {
      const ruleText = parsed.aligned ? (parsed.byIndex[i] || '') : '';
      return {
        index: i + 1,
        score: blankScore,
        accepted: ruleText ? extractAcceptedAnswers(ruleText, blankScore) : [],
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

  return {
    paper: {
      paper_id: paperId,
      title: edition?.name || '未命名试卷',
      usage: edition?.purpose || '',
      total_score: grading?.total ?? computedTotal,
      page_count: pageCount,
      student_id_digits: studentIdDigits,
      layout: {
        logical_page_mm: [210, 297],
        qr_position: 'top-right',
        qr_size_mm: 18,
        anchor_size_mm: 4,
        anchor_inset_mm: 6
      },
      generated_at: generatedAt
    },
    questions
  };
}

// ---------- Shared A4 Student Layout & Real-DOM Pagination ----------

const GRADING_A4_PRINT_CSS = `
@page { size: A4; margin: 0; }
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; background: #fff; color: #111; }
body { font: 10.5pt/1.62 "Songti SC", "SimSun", "Noto Serif CJK SC", "Times New Roman", serif; -webkit-print-color-adjust: exact; print-color-adjust: exact; }
.a4-page { width: 210mm; height: 297mm; padding: 12mm 16mm 13mm 16mm; display: flex; flex-direction: column; overflow: hidden; page-break-after: always; break-after: page; position: relative; }
.a4-page:last-child { page-break-after: auto; break-after: auto; }
.reg-mark { position: absolute; width: 4mm; height: 4mm; background: #000; pointer-events: none; z-index: 5; }
.reg-tl { top: 6mm; left: 6mm; }
.reg-tr { top: 6mm; right: 6mm; }
.reg-bl { bottom: 6mm; left: 6mm; }
.reg-br { bottom: 6mm; right: 6mm; }
.page-hdr { flex: none; display: flex; align-items: flex-end; justify-content: space-between; gap: 6mm; min-height: 26mm; padding-bottom: 2mm; margin-bottom: 3mm; border-bottom: 0.5pt solid #888; }
.hdr-school { flex: 1; min-width: 0; font-size: 9.5pt; color: #333; line-height: 1.4; padding-bottom: 1mm; }
.hdr-qr { flex: none; display: flex; flex-direction: column; align-items: center; text-align: center; }
.hdr-qr svg { display: block; width: 18mm; height: 18mm; }
.hdr-qr-code { font-family: ui-monospace, "SFMono-Regular", Menlo, Consolas, "Courier New", monospace; font-size: 6.5pt; line-height: 1.25; color: #111; margin-top: 0.7mm; white-space: nowrap; }
.hdr-qr-meta { font-size: 6.5pt; line-height: 1.25; color: #444; white-space: nowrap; }
.page-body { flex: 1; min-height: 0; overflow: hidden; }
.page-ftr { flex: none; text-align: center; font-size: 8.5pt; color: #333; padding-top: 2mm; margin-top: 2mm; border-top: 0.4pt solid #bbb; height: 8mm; line-height: 5.5mm; }
.paper-head { text-align: center; margin-bottom: 9pt; padding-bottom: 6pt; border-bottom: 0.8pt solid #999; }
.paper-head h1 { margin: 0 0 3pt; font-size: 15.5pt; font-weight: 700; line-height: 1.3; }
.paper-meta { margin: 2pt 0 5pt; font-size: 9.5pt; color: #333; }
.student-bar { display: flex; flex-wrap: wrap; align-items: center; justify-content: center; gap: 4mm 6mm; margin: 5pt 0 2pt; font-size: 10pt; color: #111; }
.student-bar.two-row { flex-direction: column; gap: 3mm; }
.student-fields { display: inline-flex; align-items: baseline; gap: 6mm; }
.fill-line { display: inline-block; width: 24mm; border-bottom: 0.7pt solid #111; transform: translateY(-0.6mm); }
.id-box-group { display: inline-flex; align-items: center; gap: 1.8mm; }
.id-label { margin-right: 0.5mm; }
.id-box { display: inline-block; width: 6mm; height: 6mm; border: 0.35mm solid #000; background: #fff; }
.section-head { margin: 10pt 0 6pt; font-size: 11.5pt; font-weight: 700; font-family: "Kaiti SC", "KaiTi", "STKaiti", serif; }
.q-item { margin: 0 0 9pt; }
.q-body { display: block; }
.q-no { font-weight: 600; float: left; margin-right: 4pt; }
.doc p { margin: 0 0 3.5pt; white-space: pre-wrap; }
.doc p:first-child { display: inline; }
.doc p:first-child::after { content: ""; display: block; }
.doc img { display: block; max-width: 100%; max-height: 92mm; height: auto; margin: 4pt 0; object-fit: contain; }
.doc table { border-collapse: collapse; margin: 5pt 0; max-width: 100%; }
.doc td, .doc th { border: 0.6pt solid #444; padding: 2.5pt 6pt; font-size: 9.5pt; }
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

async function prepareStudentFlowUnits(edition, items) {
  const grading = edition?.grading;
  const headings = new Map(paperSections(items.map(it => it.q), grading).map(g => [g.questions[0].id, g.title]));
  const units = [];
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    if (headings.has(item.q.id)) {
      const title = headings.get(item.q.id);
      units.push({ kind: 'section', title, html: `<h2 class="section-head">${escapeDoc(title)}</h2>` });
    }
    const rule = grading?.items?.[item.q.id];
    const rawFace = basketSection(item.q) < 3
      ? blankPrintedAnswer(scoredFace(item.q, grading), shortAnswer(item.answer))
      : scoredFace(item.q, grading);
    const face = await inlineHtmlPictures(rawFace);
    const prefix = `${i + 1}．` + (rule?.kind === 3 ? `（${rule.points}分）` : '');
    units.push({
      kind: 'question',
      qIndex: i,
      item,
      prefix,
      faceHtml: rawFace,
      html: `<article class="q-item"><div class="q-body"><span class="q-no">${escapeDoc(prefix)}</span><div class="doc">${face}</div></div></article>`
    });
  }
  return units;
}

// Measures real A4 height in a hidden offscreen iframe so PDF print pages and
// grading pack JSON page numbers align identically.
async function paginateStudentPaper(edition, items, opts = {}) {
  const digits = normalizeStudentIdDigits(opts.studentIdDigits);
  const safetyRatio = opts.safetyRatio ?? 0.98;
  const units = opts.units || await prepareStudentFlowUnits(edition, items);
  if (typeof document === 'undefined' || !document.body) {
    return { pageCount: null, pages: [{ pageNo: 1, units }], questionPages: null, units };
  }
  const leadHtml = buildStudentLeadHtml(edition, items, digits);
  const measureHtml = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<style>${GRADING_A4_PRINT_CSS}
#measure-pool { position: absolute; left: 0; top: 0; width: 178mm; visibility: hidden; }
</style></head><body>
<section class="a4-page" id="ref-page">
  <div class="page-hdr"><div class="hdr-school">${escapeDoc(opts.schoolHeader || '')}</div><div class="hdr-qr"><svg viewBox="0 0 27 27" width="18mm" height="18mm"></svg><div class="hdr-qr-code">BF:0000000000:1</div><div class="hdr-qr-meta">生物试卷 · 第 1 页</div></div></div>
  <div class="page-body" id="ref-body"></div>
  <footer class="page-ftr">第 1 页　共 1 页</footer>
</section>
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
    const refBody = doc.getElementById('ref-body');
    const refBodyH = refBody?.clientHeight || 0;
    if (refBodyH < 200) return { pageCount: null, pages: [{ pageNo: 1, units }], questionPages: null, units };
    const usable = Math.floor(refBodyH * safetyRatio);
    const leadNode = doc.getElementById('m-lead')?.firstElementChild;
    const unitNodes = units.map((_, idx) => doc.querySelector(`[data-u="${idx}"]`)?.firstElementChild);

    // Pack directly inside the real .page-body container so margin collapsing,
    // floats, and line heights match final A4 print layout to the exact pixel.
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

      // Prevent orphan section heading at the bottom of a page by testing heading + first question together.
      if (u.kind === 'section' && i + 1 < units.length && units[i + 1].kind === 'question' && unitNodes[i + 1]) {
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
    for (const p of pages) {
      for (const u of p.units) {
        if (u.kind === 'question') questionPages.set(u.qIndex, p.pageNo);
      }
    }
    return { pageCount: pages.length, pages, questionPages, units };
  } catch {
    return { pageCount: null, pages: [{ pageNo: 1, units }], questionPages: null, units };
  } finally {
    frame.remove();
  }
}

async function buildGradingPrintHtml(edition, items, opts = {}) {
  const paperId = opts.paperId || await computePaperId(edition);
  const digits = normalizeStudentIdDigits(opts.studentIdDigits);
  const schoolHeader = String(opts.schoolHeader || '').trim();
  const layout = opts.layout || await paginateStudentPaper(edition, items, {
    studentIdDigits: digits,
    schoolHeader,
    safetyRatio: 0.98
  });
  const totalPages = layout.pages.length;
  const leadHtml = buildStudentLeadHtml(edition, items, digits);
  const shortTitle = abbreviatePaperTitle(edition?.name || '生物试卷', 12);

  const pageBlocks = layout.pages.map(p => {
    const code = formatQrPayload(paperId, p.pageNo);
    const svg = qrSvgString(code, { quiet: 3, ecLevel: 'Q', sizeMm: 18 });
    const bodyHtml = (p.pageNo === 1 ? leadHtml : '') + p.units.map(u => u.html).join('');
    return `<section class="a4-page" data-page="${p.pageNo}">
      <span class="reg-mark reg-tl" aria-hidden="true"></span>
      <span class="reg-mark reg-tr" aria-hidden="true"></span>
      <span class="reg-mark reg-bl" aria-hidden="true"></span>
      <span class="reg-mark reg-br" aria-hidden="true"></span>
      <header class="page-hdr">
        <div class="hdr-school">${escapeDoc(schoolHeader)}</div>
        <div class="hdr-qr">
          ${svg}
          <div class="hdr-qr-code">${escapeDoc(code)}</div>
          <div class="hdr-qr-meta">${escapeDoc(shortTitle)} · 第 ${p.pageNo} 页</div>
        </div>
      </header>
      <div class="page-body">${bodyHtml}</div>
      <footer class="page-ftr">第 ${p.pageNo} 页　共 ${totalPages} 页</footer>
    </section>`;
  });

  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:;">
<title>${escapeDoc(edition?.name || '生物试卷')}</title>
<style>${GRADING_A4_PRINT_CSS}</style>${'</' + 'head>'}<body>${pageBlocks.join('')}</body></html>`;
}

// ---------- Grading Word (.docx) Templates, Student-ID Boxes & Per-Page QR ----------

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
          width: { size: 102, type: D.WidthType.DXA }, // 1.8 mm gap
          borders: noneBorders,
          margins: zeroMargins,
          children: [new D.Paragraph({ spacing: { before: 0, after: 0 }, children: [] })]
        }));
      }
      cells.push(new D.TableCell({
        width: { size: 340, type: D.WidthType.DXA }, // 6 mm square
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
      if (i > 0) widths.push(102); // 1.8 mm gap
      widths.push(340); // 6 mm square
    }
    return widths;
  };

  if (digits <= 8) {
    const labelW = 5000; // ~88 mm: fits 班级/姓名/学号 cleanly on one single line
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
      new D.Paragraph({ spacing: { before: 0, after: 120 }, children: [] })
    ];
  }

  // Wide student IDs (9-16 digits): put class/name on row 1 and digit boxes centered on row 2.
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
      spacing: { before: 0, after: 80 },
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
    new D.Paragraph({ spacing: { before: 0, after: 120 }, children: [] })
  ];
}

async function makeGradingBaseTemplate(schoolHeader = '', studentMode = true) {
  await loadTool('docx');
  const D = docx, C = D.AlignmentType.CENTER;
  const line = (text, opts = {}) => new D.Paragraph({
    alignment: opts.align,
    spacing: { after: opts.after ?? 80 },
    children: [new D.TextRun({ text, bold: opts.bold, size: opts.size, font: opts.font || WORD_BODY_FONT })]
  });
  const hdrText = String(schoolHeader || '').trim();
  const children = [
    line('{{试卷名称}}', { align: C, bold: true, size: 32, after: 100 }),
    ...(studentMode ? [line('{{学号方框}}', { align: C, after: 80 })] : []),
    line('{{试题}}', { after: 0 })
  ];
  return D.Packer.toBlob(new D.Document({
    styles: { default: { document: { run: { font: WORD_BODY_FONT, size: 24 } } } },
    sections: [{
      properties: {
        page: {
          size: { width: 11906, height: 16838 },
          margin: { top: studentMode ? 2160 : 1080, bottom: 1134, left: 1134, right: 1134, header: 680, footer: 680 }
        }
      },
      headers: hdrText ? { default: new D.Header({ children: [line(hdrText, { align: C, size: 19, after: 0 })] }) } : {},
      footers: {
        default: new D.Footer({
          children: [new D.Paragraph({
            alignment: C,
            children: [new D.TextRun({ children: ['第 ', D.PageNumber.CURRENT, ' 页　共 ', D.PageNumber.TOTAL_PAGES, ' 页'], size: 18, font: WORD_BODY_FONT })]
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
    spacing: { after: opts.after ?? 80 },
    children: [new D.TextRun({ text, bold: opts.bold, size: opts.size, font: opts.font || WORD_BODY_FONT })]
  });
  const hdrText = String(schoolHeader || '').trim() || '××学校 2026—2027 学年第一学期';
  return D.Packer.toBlob(new D.Document({
    styles: { default: { document: { run: { font: WORD_BODY_FONT, size: 24 } } } },
    sections: [{
      properties: {
        page: {
          size: { width: 11906, height: 16838 },
          margin: { top: 2160, bottom: 1134, left: 1134, right: 1134, header: 680, footer: 680 }
        }
      },
      headers: { default: new D.Header({ children: [line(hdrText, { align: C, size: 19, after: 0 })] }) },
      footers: {
        default: new D.Footer({
          children: [new D.Paragraph({
            alignment: C,
            children: [new D.TextRun({ children: ['第 ', D.PageNumber.CURRENT, ' 页　共 ', D.PageNumber.TOTAL_PAGES, ' 页'], size: 18, font: WORD_BODY_FONT })]
          })]
        })
      },
      children: [
        line('{{试卷名称}}', { align: C, bold: true, size: 32, after: 100 }),
        line('{{学号方框}}', { align: C, after: 80 }),
        line('注意事项：选择题请在题干括号内工整填写选项字母；学号请逐位写在上方方框内。', { size: 19, after: 140 }),
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

function buildQrHeaderXml(pageNo, qrCodeText, schoolHeader, shortTitle, pageSizeEmu = null) {
  const cx = 648000; // 18 mm in EMU (18 * 36000)
  const docPrId = 1000 + pageNo;
  const picId = 2000 + pageNo;
  const pageW = pageSizeEmu?.w || 7560310; // A4 width (11906 dxa * 635)
  const pageH = pageSizeEmu?.h || 10692130; // A4 height (16838 dxa * 635)
  const inset = 216000; // 6 mm inset from page edges
  const anchorSize = 144000; // 4 mm solid square
  const aBase = 3000 + pageNo * 10;
  const anchorsXml = [
    buildCornerAnchorRunXml(aBase + 1, `BFAnchorTL_${pageNo}`, inset, inset, 'rId2'),
    buildCornerAnchorRunXml(aBase + 2, `BFAnchorTR_${pageNo}`, pageW - inset - anchorSize, inset, 'rId2'),
    buildCornerAnchorRunXml(aBase + 3, `BFAnchorBL_${pageNo}`, inset, pageH - inset - anchorSize, 'rId2'),
    buildCornerAnchorRunXml(aBase + 4, `BFAnchorBR_${pageNo}`, pageW - inset - anchorSize, pageH - inset - anchorSize, 'rId2')
  ].join('');
  const schoolLines = String(schoolHeader || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  const leftParas = schoolLines.length
    ? schoolLines.map(line => `<w:p><w:pPr><w:spacing w:before="0" w:after="20"/></w:pPr><w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:color w:val="333333"/><w:sz w:val="19"/><w:szCs w:val="19"/></w:rPr><w:t xml:space="preserve">${escapeXmlText(line)}</w:t></w:r></w:p>`).join('')
    : '<w:p><w:pPr><w:spacing w:before="0" w:after="0"/></w:pPr></w:p>';

  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">
  <w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="20" w:lineRule="exact"/></w:pPr>${anchorsXml}</w:p>
  <w:tbl>
    <w:tblPr>
      <w:tblW w:w="5000" w:type="pct"/>
      <w:tblBorders>
        <w:top w:val="none" w:sz="0" w:space="0" w:color="auto"/>
        <w:left w:val="none" w:sz="0" w:space="0" w:color="auto"/>
        <w:bottom w:val="single" w:sz="4" w:space="0" w:color="999999"/>
        <w:right w:val="none" w:sz="0" w:space="0" w:color="auto"/>
        <w:insideH w:val="none" w:sz="0" w:space="0" w:color="auto"/>
        <w:insideV w:val="none" w:sz="0" w:space="0" w:color="auto"/>
      </w:tblBorders>
      <w:tblCellMar>
        <w:top w:w="0" w:type="dxa"/><w:left w:w="0" w:type="dxa"/>
        <w:bottom w:w="40" w:type="dxa"/><w:right w:w="0" w:type="dxa"/>
      </w:tblCellMar>
    </w:tblPr>
    <w:tr>
      <w:tc>
        <w:tcPr><w:tcW w:w="3600" w:type="pct"/><w:vAlign w:val="bottom"/></w:tcPr>
        ${leftParas}
      </w:tc>
      <w:tc>
        <w:tcPr><w:tcW w:w="1400" w:type="pct"/><w:vAlign w:val="bottom"/></w:tcPr>
        <w:p>
          <w:pPr><w:jc w:val="center"/><w:spacing w:before="0" w:after="20"/></w:pPr>
          <w:r>
            <w:drawing>
              <wp:inline distT="0" distB="0" distL="0" distR="0">
                <wp:extent cx="${cx}" cy="${cx}"/>
                <wp:effectExtent l="0" t="0" r="0" b="0"/>
                <wp:docPr id="${docPrId}" name="BFQR_${pageNo}"/>
                <wp:cNvGraphicFramePr><a:graphicFrameLocks noChangeAspect="1"/></wp:cNvGraphicFramePr>
                <a:graphic>
                  <a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">
                    <pic:pic>
                      <pic:nvPicPr><pic:cNvPr id="${picId}" name="bf_qr_p${pageNo}.png"/><pic:cNvPicPr/></pic:nvPicPr>
                      <pic:blipFill><a:blip r:embed="rId1"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>
                      <pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="${cx}" cy="${cx}"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>
                    </pic:pic>
                  </a:graphicData>
                </a:graphic>
              </wp:inline>
            </w:drawing>
          </w:r>
        </w:p>
        <w:p>
          <w:pPr><w:jc w:val="center"/><w:spacing w:before="0" w:after="10" w:line="160" w:lineRule="auto"/></w:pPr>
          <w:r><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Courier New" w:eastAsia="Consolas"/><w:color w:val="111111"/><w:sz w:val="13"/><w:szCs w:val="13"/></w:rPr><w:t>${escapeXmlText(qrCodeText)}</w:t></w:r>
        </w:p>
        <w:p>
          <w:pPr><w:jc w:val="center"/><w:spacing w:before="0" w:after="0" w:line="160" w:lineRule="auto"/></w:pPr>
          <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:color w:val="555555"/><w:sz w:val="13"/><w:szCs w:val="13"/></w:rPr><w:t>${escapeXmlText(shortTitle + ' · 第 ' + pageNo + ' 页')}</w:t></w:r>
        </w:p>
      </w:tc>
    </w:tr>
  </w:tbl>
</w:hdr>`;
}

function buildDefaultPageFooterXml() {
  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:p>
    <w:pPr><w:jc w:val="center"/><w:spacing w:before="0" w:after="0"/></w:pPr>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr><w:t xml:space="preserve">第 </w:t></w:r>
    <w:fldSimple w:instr=" PAGE "><w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr><w:t xml:space="preserve"> 页　共 </w:t></w:r>
    <w:fldSimple w:instr=" NUMPAGES "><w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:t>1</w:t></w:r></w:fldSimple>
    <w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr><w:t xml:space="preserve"> 页</w:t></w:r>
  </w:p>
</w:ftr>`;
}

// Extracts plain header text from an uploaded custom .docx template so grading headers
// keep the school's header text alongside the top-right QR code.
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
      Array.from(p.getElementsByTagNameNS(ns, 't'), t => t.textContent).join('').trim()
    ).filter(Boolean);
    return paras.join('\n');
  } catch {
    return '';
  }
}

// Post-process a patched .docx blob: convert __BF_GRADING_PAGE_BREAK_k__ markers into
// OOXML section breaks and attach unique top-right 18x18mm QR headers per page.
async function stampGradingWordPages(docxBlob, opts) {
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
  if (!body) throw Error('无法写入批改版页眉，Word 正文结构异常。');

  const relsPath = 'word/_rels/document.xml.rels';
  const relsXml = new DOMParser().parseFromString(
    zip.file(relsPath) ? await zip.file(relsPath).async('string') : `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="${relNs}"/>`,
    'application/xml'
  );
  const ctXml = new DOMParser().parseFromString(await zip.file('[Content_Types].xml').async('string'), 'application/xml');

  const markerParas = [];
  for (const child of Array.from(body.children)) {
    if (child.localName !== 'p') continue;
    const text = Array.from(child.getElementsByTagNameNS(wNs, 't'), t => t.textContent).join('').trim();
    if (text.startsWith(GRADING_PAGE_BREAK_PREFIX)) markerParas.push(child);
  }
  const pageCount = markerParas.length + 1;

  // Ensure a base sectPr exists at the end of body.
  let finalSectPr = Array.from(body.children).find(n => n.localName === 'sectPr');
  if (!finalSectPr) {
    finalSectPr = docXml.createElementNS(wNs, 'w:sectPr');
    body.appendChild(finalSectPr);
  }

  // Ensure margins keep content, 18mm QR header, and footer clear of the 4mm corner anchors (6-10mm from edges).
  let pgMar = Array.from(finalSectPr.children).find(n => n.localName === 'pgMar');
  if (!pgMar) {
    pgMar = docXml.createElementNS(wNs, 'w:pgMar');
    pgMar.setAttributeNS(wNs, 'w:top', '2160');
    pgMar.setAttributeNS(wNs, 'w:bottom', '1134');
    pgMar.setAttributeNS(wNs, 'w:left', '1134');
    pgMar.setAttributeNS(wNs, 'w:right', '1134');
    pgMar.setAttributeNS(wNs, 'w:header', '680');
    pgMar.setAttributeNS(wNs, 'w:footer', '680');
    finalSectPr.appendChild(pgMar);
  } else {
    const readMar = attr => Number(pgMar.getAttributeNS(wNs, attr) || pgMar.getAttribute('w:' + attr) || 0);
    if (readMar('top') < 2160) pgMar.setAttributeNS(wNs, 'w:top', '2160');
    if (readMar('bottom') < 1134) pgMar.setAttributeNS(wNs, 'w:bottom', '1134');
    if (readMar('left') < 907) pgMar.setAttributeNS(wNs, 'w:left', '907');
    if (readMar('right') < 907) pgMar.setAttributeNS(wNs, 'w:right', '907');
    if (readMar('header') < 680) pgMar.setAttributeNS(wNs, 'w:header', '680');
    if (readMar('footer') < 680) pgMar.setAttributeNS(wNs, 'w:footer', '680');
  }
  const pgSz = Array.from(finalSectPr.children).find(n => n.localName === 'pgSz');
  const pgW = Number(pgSz?.getAttributeNS(wNs, 'w') || pgSz?.getAttribute('w:w') || 11906);
  const pgH = Number(pgSz?.getAttributeNS(wNs, 'h') || pgSz?.getAttribute('w:h') || 16838);
  const pageSizeEmu = { w: pgW * 635, h: pgH * 635 };

  // Check if template already has a default footer; if not, attach our standard page-number footer.
  const hasFooterRef = Array.from(finalSectPr.children).some(n => n.localName === 'footerReference');
  let defaultFooterRid = null;
  if (!hasFooterRef) {
    defaultFooterRid = 'rIdBfFtr1';
    zip.file('word/footer_bf.xml', buildDefaultPageFooterXml());
    const fRel = relsXml.createElementNS(relNs, 'Relationship');
    fRel.setAttribute('Id', defaultFooterRid);
    fRel.setAttribute('Type', 'http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer');
    fRel.setAttribute('Target', 'footer_bf.xml');
    relsXml.documentElement.appendChild(fRel);
    const fOv = ctXml.createElementNS(ctNs, 'Override');
    fOv.setAttribute('PartName', '/word/footer_bf.xml');
    fOv.setAttribute('ContentType', 'application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml');
    ctXml.documentElement.appendChild(fOv);
  }

  // Ensure PNG content type is declared.
  const hasPngType = Array.from(ctXml.getElementsByTagNameNS(ctNs, 'Default'))
    .some(el => String(el.getAttribute('Extension') || '').toLowerCase() === 'png');
  if (!hasPngType) {
    const def = ctXml.createElementNS(ctNs, 'Default');
    def.setAttribute('Extension', 'png');
    def.setAttribute('ContentType', 'image/png');
    ctXml.documentElement.appendChild(def);
  }

  const shortTitle = abbreviatePaperTitle(opts.title || '生物试卷', 12);
  const schoolHeader = opts.schoolHeader || '';
  const paperId = opts.paperId;

  const configureSectPr = (sectPr, pageNo, isNextPage) => {
    for (const child of Array.from(sectPr.children)) {
      if (['headerReference', 'footerReference', 'titlePg', 'type'].includes(child.localName)) {
        if (child.localName === 'footerReference' && !defaultFooterRid) continue;
        sectPr.removeChild(child);
      }
    }
    const anchor = sectPr.firstChild;
    const hdrRef = docXml.createElementNS(wNs, 'w:headerReference');
    hdrRef.setAttributeNS(wNs, 'w:type', 'default');
    hdrRef.setAttributeNS(rNs, 'r:id', 'rIdBfHdr' + pageNo);
    sectPr.insertBefore(hdrRef, anchor);
    if (defaultFooterRid) {
      const ftrRef = docXml.createElementNS(wNs, 'w:footerReference');
      ftrRef.setAttributeNS(wNs, 'w:type', 'default');
      ftrRef.setAttributeNS(rNs, 'r:id', defaultFooterRid);
      sectPr.insertBefore(ftrRef, anchor);
    }
    if (isNextPage) {
      // OOXML CT_SectPr schema requires <w:type> after header/footerReference and before <w:pgSz>/<w:pgMar>.
      const firstBodyProp = Array.from(sectPr.children).find(n => !['headerReference', 'footerReference', 'footnotePr', 'endnotePr'].includes(n.localName));
      const tp = docXml.createElementNS(wNs, 'w:type');
      tp.setAttributeNS(wNs, 'w:val', 'nextPage');
      sectPr.insertBefore(tp, firstBodyProp || null);
    }
  };

  // Shared 4mm solid black corner anchor PNG referenced by every page header (rId2).
  zip.file('word/media/bf_anchor.png', solidBlackSquarePngBytes(32));

  for (let p = 1; p <= pageCount; p++) {
    const code = formatQrPayload(paperId, p);
    const png = qrPngBytes(code, { quiet: 3, scale: 8, ecLevel: 'Q' });
    zip.file(`word/media/bf_qr_p${p}.png`, png.bytes);
    zip.file(`word/header_bf_${p}.xml`, buildQrHeaderXml(p, code, schoolHeader, shortTitle, pageSizeEmu));
    zip.file(`word/_rels/header_bf_${p}.xml.rels`, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="${relNs}">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/bf_qr_p${p}.png"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/bf_anchor.png"/>
</Relationships>`);

    const hRel = relsXml.createElementNS(relNs, 'Relationship');
    hRel.setAttribute('Id', 'rIdBfHdr' + p);
    hRel.setAttribute('Type', 'http://schemas.openxmlformats.org/officeDocument/2006/relationships/header');
    hRel.setAttribute('Target', `header_bf_${p}.xml`);
    relsXml.documentElement.appendChild(hRel);

    const hOv = ctXml.createElementNS(ctNs, 'Override');
    hOv.setAttribute('PartName', `/word/header_bf_${p}.xml`);
    hOv.setAttribute('ContentType', 'application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml');
    ctXml.documentElement.appendChild(hOv);
  }

  // Convert each break marker paragraph into a section break closing page k.
  for (let idx = 0; idx < markerParas.length; idx++) {
    const pageNo = idx + 1;
    const marker = markerParas[idx];
    const sectClone = finalSectPr.cloneNode(true);
    configureSectPr(sectClone, pageNo, pageNo > 1);
    const brkP = docXml.createElementNS(wNs, 'w:p');
    const pPr = docXml.createElementNS(wNs, 'w:pPr');
    const spacing = docXml.createElementNS(wNs, 'w:spacing');
    spacing.setAttributeNS(wNs, 'w:before', '0');
    spacing.setAttributeNS(wNs, 'w:after', '0');
    spacing.setAttributeNS(wNs, 'w:line', '20');
    spacing.setAttributeNS(wNs, 'w:lineRule', 'exact');
    pPr.appendChild(spacing);
    pPr.appendChild(sectClone);
    brkP.appendChild(pPr);
    body.replaceChild(brkP, marker);
  }

  // Final section governs the last page.
  configureSectPr(finalSectPr, pageCount, pageCount > 1);

  // Disable evenAndOddHeaders in word/settings.xml if present so every page uses default header.
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
