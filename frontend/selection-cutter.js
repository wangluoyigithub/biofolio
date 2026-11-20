// Whole-paper visual PDF & photo question cutter workbench (pure client-side).
const CUT_TYPES = ['单选题', '多选题', '判断题', '实验题', '填空题', '非选择题'];
const cutterPageCache = new Map();
const cutterStitchCache = new Map();

function normalizeCutQuestionNo(raw) {
  const s = String(raw ?? '').trim().replace(/^第\s*/, '').replace(/\s*题$/, '').replace(/[.．、:：\s]+$/g, '').trim();
  const example = s.match(/^例\s*0*(\d+)$/);
  if (example) return '例' + Number(example[1]);
  const num = s.match(/^0*(\d+)$/);
  if (num) return String(Number(num[1]));
  return s;
}

function nextCutQuestionLabel(items, storedNumbers) {
  let maxN = 0;
  for (const v of storedNumbers || []) {
    const n = Number(normalizeCutQuestionNo(v));
    if (Number.isInteger(n) && n > maxN) maxN = n;
  }
  for (const it of items || []) {
    const n = Number(normalizeCutQuestionNo(it.label));
    if (Number.isInteger(n) && n > maxN) maxN = n;
  }
  return String(maxN + 1);
}

function isUsablePdfText(text) {
  const compact = String(text || '').replace(/\s+/g, '');
  if (compact.length < 12) return false;
  const meaningful = (compact.match(/[\u4e00-\u9fffA-Za-z0-9]/g) || []).length;
  if (meaningful < 6) return false;
  const bad = (compact.match(/[\uE000-\uF8FF\uFFFD\u0000-\u001F]/g) || []).length;
  return bad / compact.length < 0.08;
}

// Extract text items inside normalized [0..1] top-left box from a pdf.js page textContent + viewport(scale=1).
function extractPdfRegionText(textItems, viewport, box) {
  if (!Array.isArray(textItems) || !viewport || !box) return '';
  const vw = Math.max(1, viewport.width), vh = Math.max(1, viewport.height);
  const padX = 0.01, padY = 0.012;
  const x0 = box.x - padX, x1 = box.x + box.w + padX;
  const y0 = box.y - padY, y1 = box.y + box.h + padY;
  const picked = [];
  for (const it of textItems) {
    if (typeof it?.str !== 'string' || !it.str) continue;
    const tr = it.transform || [1, 0, 0, 1, 0, 0];
    const fh = Math.hypot(tr[2], tr[3]) || it.height || 10;
    const pt = typeof viewport.convertToViewportPoint === 'function'
      ? viewport.convertToViewportPoint(tr[4], tr[5])
      : [tr[4], vh - tr[5]];
    const nx = pt[0] / vw;
    const ny = (pt[1] - fh * 0.32) / vh;
    const nw = Math.max(0, (it.width || 0) / vw);
    const overlapsX = nx <= x1 && (nx + nw) >= x0;
    const overlapsY = ny >= y0 && ny <= y1;
    if (overlapsX && overlapsY) {
      picked.push({ nx, ny, endX: nx + nw, h: fh / vh, str: it.str });
    }
  }
  if (!picked.length) return '';
  picked.sort((a, b) => a.ny - b.ny || a.nx - b.nx);
  const lines = [];
  for (const it of picked) {
    let line = lines.find(l => Math.abs(l.y - it.ny) < Math.max(l.h, it.h, 0.008) * 0.55);
    if (!line) { line = { y: it.ny, h: it.h, parts: [] }; lines.push(line); }
    line.parts.push(it);
  }
  lines.sort((a, b) => a.y - b.y);
  let maxEnd = 0;
  for (const l of lines) {
    l.parts.sort((a, b) => a.nx - b.nx);
    let text = '';
    for (let i = 0; i < l.parts.length; i++) {
      const p = l.parts[i];
      if (i > 0) {
        const prev = l.parts[i - 1];
        const gap = p.nx - prev.endX;
        if (gap > Math.max(p.h, 0.006) * 0.35 && !/\s$/.test(text) && !/^\s/.test(p.str)) text += ' ';
      }
      text += p.str;
    }
    l.text = text.trim();
    l.end = Math.max(...l.parts.map(p => p.endX));
    if (l.end > maxEnd) maxEnd = l.end;
  }
  const paragraphs = [];
  let prev = null;
  for (const l of lines.filter(l => l.text)) {
    const startsNew = /^(?:例\s*\d+|\d+\s*[.．、]|[A-H]\s*[.．、]|[（(]\s*\d+\s*[）)]|[①②③④⑤⑥⑦⑧⑨⑩])/.test(l.text);
    const prevFull = prev && prev.end >= maxEnd - Math.max(prev.h, 0.012) * 2.2;
    if (prev && prevFull && !startsNew) {
      prev.text += (/^[A-Za-z0-9]/.test(l.text) && /[A-Za-z0-9]$/.test(prev.text) ? ' ' : '') + l.text;
      prev.end = l.end;
    } else {
      prev = { text: l.text, end: l.end, h: l.h };
      paragraphs.push(prev);
    }
  }
  return paragraphs.map(p => p.text).join('\n');
}

function parseVisionCutResponse(reply) {
  const raw = String(reply || '').trim().replace(/^```(?:json)?\s*|\s*```$/gi, '');
  let data = null;
  try {
    data = JSON.parse(raw);
  } catch {
    if (!raw || raw === 'UNREADABLE') throw Error('未读到清晰题面，请调整选区后重试。');
    return { number: '', type: '', text: sciText(raw), source: printedQuestionSource(raw), hasFigure: false };
  }
  const text = typeof data?.text === 'string' ? sciText(data.text.trim()) : '';
  if (!text) throw Error('AI 未返回有效题面文字，请调整选区或手动输入。');
  const rawType = String(data.type || '').trim();
  const type = CUT_TYPES.includes(rawType) ? rawType : '';
  const number = data.number ? normalizeCutQuestionNo(data.number) : '';
  const source = typeof photoSource === 'function' ? photoSource(text, data.source) : printedQuestionSource(text);
  return { number, type, text, source, hasFigure: data.hasFigure === true };
}

function parseAnswerRegionJson(reply) {
  const raw = String(reply || '').trim().replace(/^```(?:json)?\s*|\s*```$/gi, '');
  const parsed = JSON.parse(raw);
  const list = Array.isArray(parsed) ? parsed : (Array.isArray(parsed?.answers) ? parsed.answers : []);
  return list.map(row => {
    const no = normalizeCutQuestionNo(row?.number ?? row?.no ?? row?.q ?? '');
    const answer = typeof (row?.answer ?? row?.ans) === 'string' ? sciText(String(row.answer ?? row.ans).trim()) : '';
    const analysis = typeof (row?.analysis ?? row?.explain) === 'string' ? sciText(String(row.analysis ?? row.explain).trim()) : '';
    return { number: no, answer, analysis };
  }).filter(r => r.number && (r.answer || r.analysis));
}

function serializeCutterDraftData(paperId, version, origin, lastPage, items, answerRegions) {
  const cleanRegion = r => ({
    page: Math.max(1, Number(r.page) || 1),
    x: Number(Number(r.x).toFixed(4)),
    y: Number(Number(r.y).toFixed(4)),
    w: Number(Number(r.w).toFixed(4)),
    h: Number(Number(r.h).toFixed(4))
  });
  const payload = {
    paper: paperId,
    version,
    kind: 'cutter',
    origin: origin || '',
    lastPage: lastPage || 1,
    items: (items || []).map(it => ({
      id: it.id,
      label: String(it.label || ''),
      type: it.type || '单选题',
      source: it.source || '',
      regions: (it.regions || []).map(cleanRegion),
      figures: (it.figures || []).map(f => ({
        id: f.id,
        ...cleanRegion(f),
        enhanced: !!f.enhanced,
        redrawnData: (typeof f.redrawnData === 'string' && f.redrawnData.length <= 520000) ? f.redrawnData : ''
      })),
      text: it.text || '',
      textMode: it.textMode || '',
      hasFigureHint: !!it.hasFigureHint,
      answer: it.answer || '',
      analysis: it.analysis || '',
      aiDraft: it.aiDraft ? { answer: it.aiDraft.answer || '', analysis: it.aiDraft.analysis || '' } : null,
      aiNote: it.aiNote || '',
      stored: !!it.stored
    })),
    answerRegions: (answerRegions || []).map(a => ({ id: a.id, ...cleanRegion(a) }))
  };
  let text = JSON.stringify(payload);
  if (text.length > 3500000) {
    for (const it of payload.items) for (const f of it.figures) f.redrawnData = '';
    text = JSON.stringify(payload);
  }
  return payload;
}

function buildCutQuestionRecord(paperId, paperOrigin, item, contentHtml, referencePhoto, existingQs) {
  const label = String(item.label || '').trim() || '1';
  const lines = String(item.text || '').trim().split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  const stem = lines[0] || '图片题';
  const body = String(item.text || '').trim();
  const baseId = paperId + '-' + label;
  const taken = new Set((existingQs || []).map(q => q.id));
  let qId = baseId;
  if (taken.has(qId)) {
    let idx = 2;
    while (taken.has(paperId + '-c' + idx + '-' + label)) idx++;
    qId = paperId + '-c' + idx + '-' + label;
  }
  const time = new Date(), date = time.toLocaleDateString('sv-SE');
  const q = {
    id: qId,
    stem,
    body,
    contentHtml,
    answerHtml: '',
    analysisHtml: '',
    answer: sciText(String(item.answer || '').trim()),
    a: sciText(String(item.analysis || '').trim()),
    src: [[paperId, label]],
    origin: String(item.source || '').trim() || String(paperOrigin || '').trim() || (typeof printedQuestionSource === 'function' ? printedQuestionSource(body) : ''),
    date,
    collectedAt: date + 'T' + time.toTimeString().slice(0, 8),
    type: item.type || '单选题',
    k: [],
    level: ''
  };
  if (referencePhoto) q.referencePhoto = referencePhoto;
  return q;
}

const paperCutter = {
  data() {
    return {
      cutPaperId: '',
      cutPaperTitle: '',
      cutPaperOrigin: '',
      cutSourceKind: '',
      cutPdfDoc: null,
      cutPhotoPages: [],
      cutPageCount: 0,
      cutPage: 1,
      cutPageSize: { w: 0, h: 0 },
      cutLoading: false,
      cutPageBusy: false,
      cutError: '',
      cutNotice: '',
      cutMode: 'new', // 'new' | 'append' | 'figure' | 'answer'
      cutAutoRecognize: true,
      cutAnchor: null,
      cutActiveBox: null,
      cutItems: [],
      cutSelectedId: '',
      cutAnswerRegions: [],
      cutDraftVersion: 0,
      cutDraftDirty: false,
      cutSaveTimer: null,
      cutBusy: false,
      cutRun: 0,
      cutAnswerBusy: false,
      cutBatchBusy: false,
      cutBatchNotice: '',
      cutPreviewUrl: '',
      cutFigureThumbs: {},
      cutFigureTarget: null,
      cutThumbUrls: {}
    };
  },
  computed: {
    cutCurrentItem() {
      return this.cutItems.find(it => it.id === this.cutSelectedId) || null;
    },
    cutPageRegions() {
      const boxes = [];
      for (const it of this.cutItems) {
        it.regions.forEach((r, idx) => {
          if (r.page === this.cutPage) {
            boxes.push({
              key: it.id + '-r-' + idx,
              kind: 'region',
              itemId: it.id,
              box: r,
              selected: it.id === this.cutSelectedId,
              stored: !!it.stored,
              tag: `第${it.label}题${it.regions.length > 1 ? '·段' + (idx + 1) : ''}`
            });
          }
        });
        it.figures.forEach((f, idx) => {
          if (f.page === this.cutPage) {
            boxes.push({
              key: f.id,
              kind: 'figure',
              itemId: it.id,
              box: f,
              selected: it.id === this.cutSelectedId,
              stored: !!it.stored,
              tag: `第${it.label}题·图${idx + 1}`
            });
          }
        });
      }
      this.cutAnswerRegions.forEach((a, idx) => {
        if (a.page === this.cutPage) {
          boxes.push({
            key: a.id,
            kind: 'answer',
            itemId: '',
            box: a,
            selected: false,
            stored: false,
            tag: `答案区 ${idx + 1}`
          });
        }
      });
      return boxes;
    },
    cutPageCounts() {
      const map = {};
      for (const it of this.cutItems) {
        for (const r of it.regions) map[r.page] = (map[r.page] || 0) + 1;
      }
      for (const a of this.cutAnswerRegions) map[a.page] = (map[a.page] || 0) + 1;
      return map;
    },
    cutUnrecognizedCount() {
      return this.cutItems.filter(it => !it.stored && !it.text.trim() && it.regions.length).length;
    },
    cutPendingDraftCount() {
      return this.cutItems.filter(it => !it.stored && it.text.trim() && !solutionReady({ answer: it.answer, analysis: it.analysis }) && !it.aiDraft).length;
    },
    cutReadyStoreCount() {
      return this.cutItems.filter(it => !it.stored && it.text.trim() && solutionReady({ answer: it.answer, analysis: it.analysis })).length;
    },
    cutStoredCount() {
      return this.cutItems.filter(it => it.stored).length;
    }
  },
  watch: {
    cutSelectedId() {
      this.refreshCutSelectedPreview();
    }
  },
  beforeUnmount() {
    this.closeCutter();
  },
  methods: {
    closeCutter() {
      if (this.cutSaveTimer) { clearTimeout(this.cutSaveTimer); this.cutSaveTimer = null; }
      this.cutRun++;
      this.cutBusy = false;
      this.cutAnswerBusy = false;
      this.cutBatchBusy = false;
      if (this.cutPdfDoc) {
        try { this.cutPdfDoc.destroy(); } catch {}
        this.cutPdfDoc = null;
      }
      for (const p of this.cutPhotoPages) URL.revokeObjectURL(p.url);
      this.cutPhotoPages = [];
      for (const k of [...cutterPageCache.keys()]) {
        if (k.startsWith(this.cutPaperId + ':')) cutterPageCache.delete(k);
      }
      cutterStitchCache.clear();
      this.cutPaperId = '';
      this.cutPaperTitle = '';
      this.cutPaperOrigin = '';
      this.cutSourceKind = '';
      this.cutPageCount = 0;
      this.cutPage = 1;
      this.cutPageSize = { w: 0, h: 0 };
      this.cutDraftVersion = 0;
      this.cutError = '';
      this.cutNotice = '';
      this.cutBatchNotice = '';
      this.cutAnchor = null;
      this.cutActiveBox = null;
      this.cutItems = [];
      this.cutAnswerRegions = [];
      this.cutSelectedId = '';
      this.cutPreviewUrl = '';
      this.cutFigureThumbs = {};
      this.cutThumbUrls = {};
      this.cutFigureTarget = null;
      this.cutDraftDirty = false;
    },
    async openCutterPaper(paperId, route) {
      this.closeCutter();
      this.cutPaperId = paperId;
      this.cutPaperTitle = this.paperLabel(paperId);
      this.cutLoading = true;
      this.cutError = '';
      this.cutNotice = '';
      const token = ++this.cutRun;
      try {
        const record = await accessRecords('readonly', s => s.get(paperId));
        if (token !== this.cutRun || location.hash !== route) return;
        if (!record || !record.paper) throw Error('找不到这份试卷的原始文件。');
        this.cutPaperOrigin = record.origin || '';
        if (record.photos?.length) {
          this.cutSourceKind = 'photos';
          this.cutPhotoPages = record.photos.map((file, i) => ({
            page: i + 1,
            name: file.name || `第 ${i + 1} 页`,
            url: URL.createObjectURL(file)
          }));
          this.cutPageCount = this.cutPhotoPages.length;
        } else if (record.original && (/\.pdf$/i.test(record.original.name || '') || record.original.type === 'application/pdf')) {
          this.cutSourceKind = 'pdf';
          const { pdf, options } = await loadPdfRuntime();
          const bytes = await record.original.arrayBuffer();
          const doc = await pdf.getDocument({ ...options, data: new Uint8Array(bytes) }).promise;
          if (token !== this.cutRun || location.hash !== route) { await doc.destroy(); return; }
          this.cutPdfDoc = Vue.markRaw(doc);
          this.cutPageCount = doc.numPages;
        } else {
          throw Error('只有 PDF 或照片原卷可以使用可视化框选切题。');
        }
        await this.loadCutterDraft();
        if (token !== this.cutRun) return;
        const startPage = Math.max(1, Math.min(this.cutPageCount, this.cutPage || 1));
        this.cutLoading = false;
        await this.showCutPage(startPage);
        this.refreshCutSelectedPreview();
        this.renderCutThumbnails(token);
      } catch (e) {
        if (token === this.cutRun) {
          this.cutError = e.message || '打开原卷失败';
          this.cutLoading = false;
        }
      }
    },
    async loadCutterDraft() {
      const draft = libraryServer
        ? await (await readAsset('library/review-draft?paper=' + encodeURIComponent(this.cutPaperId))).json()
        : JSON.parse(localStorage.getItem(key + ':draft:' + this.cutPaperId) || 'null');
      this.cutDraftVersion = draft?.version || 0;
      const savedNums = new Set(
        this.qs.flatMap(q => this.sourcesOf(q)).filter(([id]) => id === this.cutPaperId).map(([, n]) => normalizeCutQuestionNo(n))
      );
      if (draft?.kind === 'cutter' && Array.isArray(draft.items)) {
        this.cutItems = draft.items.map(it => ({
          id: it.id || selectionUUID(),
          label: String(it.label || ''),
          type: CUT_TYPES.includes(it.type) ? it.type : '单选题',
          source: it.source || '',
          regions: Array.isArray(it.regions) ? it.regions : [],
          figures: Array.isArray(it.figures) ? it.figures : [],
          text: it.text || '',
          textMode: it.textMode || '',
          hasFigureHint: !!it.hasFigureHint,
          answer: it.answer || '',
          analysis: it.analysis || '',
          aiDraft: it.aiDraft || null,
          aiNote: it.aiNote || '',
          matchId: '',
          matchLabel: '',
          matchNote: '',
          link: false,
          stored: !!it.stored || savedNums.has(normalizeCutQuestionNo(it.label))
        }));
        this.cutAnswerRegions = Array.isArray(draft.answerRegions) ? draft.answerRegions : [];
        if (draft.lastPage >= 1 && draft.lastPage <= this.cutPageCount) this.cutPage = draft.lastPage;
        const firstPending = this.cutItems.find(it => !it.stored) || this.cutItems[0];
        this.cutSelectedId = firstPending?.id || '';
        if (this.cutItems.length) this.cutNotice = `已恢复上次切题进度（共 ${this.cutItems.length} 题，已入库 ${this.cutStoredCount} 题）。`;
      } else {
        this.cutItems = [];
        this.cutAnswerRegions = [];
        this.cutSelectedId = '';
        this.cutPage = 1;
      }
      this.cutDraftDirty = false;
    },
    queueCutDraftSave() {
      this.cutDraftDirty = true;
      if (this.cutSaveTimer) clearTimeout(this.cutSaveTimer);
      this.cutSaveTimer = setTimeout(() => { void this.saveCutterDraft(true); }, 1400);
    },
    async saveCutterDraft(silent = false) {
      if (!this.cutPaperId) return;
      this.cutError = '';
      if (!silent) this.cutBatchNotice = '';
      if (this.cutSaveTimer) { clearTimeout(this.cutSaveTimer); this.cutSaveTimer = null; }
      const payload = serializeCutterDraftData(
        this.cutPaperId,
        this.cutDraftVersion + 1,
        this.cutPaperOrigin,
        this.cutPage,
        this.cutItems,
        this.cutAnswerRegions
      );
      try {
        if (libraryServer) await libraryPost('review-draft', payload);
        else localStorage.setItem(key + ':draft:' + this.cutPaperId, JSON.stringify(payload));
        this.cutDraftVersion = payload.version;
        this.cutDraftDirty = false;
        if (!silent) { this.cutNotice = '切题进度已保存，下次可随时继续。'; this.notify('切题进度已保存'); }
      } catch (e) {
        if (!silent) this.cutNotice = e.message;
        else this.cutError = e.message;
      }
    },
    async renderCutPageCanvas(pageNum) {
      const cacheKey = `${this.cutPaperId}:${pageNum}`;
      if (cutterPageCache.has(cacheKey)) {
        const hit = cutterPageCache.get(cacheKey);
        cutterPageCache.delete(cacheKey);
        cutterPageCache.set(cacheKey, hit);
        return hit;
      }
      let canvas;
      if (this.cutSourceKind === 'pdf' && this.cutPdfDoc) {
        const page = await this.cutPdfDoc.getPage(pageNum);
        const orig = page.getViewport({ scale: 1 });
        const scale = Math.min(2.6, 2200 / Math.max(orig.width, orig.height));
        const vp = page.getViewport({ scale });
        canvas = document.createElement('canvas');
        canvas.width = Math.ceil(vp.width);
        canvas.height = Math.ceil(vp.height);
        await page.render({ canvasContext: canvas.getContext('2d'), viewport: vp }).promise;
      } else {
        const photo = this.cutPhotoPages[pageNum - 1];
        if (!photo) throw Error('页码超出范围');
        const img = new Image();
        img.src = photo.url;
        await new Promise((resolve, reject) => {
          img.onload = resolve;
          img.onerror = () => reject(Error('照片读取失败'));
          if (img.complete && img.naturalWidth) resolve();
        });
        canvas = document.createElement('canvas');
        canvas.width = img.naturalWidth;
        canvas.height = img.naturalHeight;
        canvas.getContext('2d').drawImage(img, 0, 0);
      }
      // LRU keep max 4 full-res pages in memory.
      cutterPageCache.set(cacheKey, canvas);
      if (cutterPageCache.size > 4) {
        const oldest = cutterPageCache.keys().next().value;
        cutterPageCache.delete(oldest);
      }
      return canvas;
    },
    async showCutPage(num) {
      if (num < 1 || num > this.cutPageCount) return;
      this.cutPage = num;
      this.cutPageBusy = true;
      this.cutError = '';
      const token = this.cutRun;
      try {
        const src = await this.renderCutPageCanvas(num);
        if (token !== this.cutRun || this.cutPage !== num) return;
        this.cutPageSize = { w: src.width, h: src.height };
        await this.$nextTick();
        const target = this.$refs.cutCanvas;
        if (!target) return;
        target.width = src.width;
        target.height = src.height;
        target.getContext('2d').drawImage(src, 0, 0);
        // Prewarm adjacent page quietly for snappy flipping.
        if (num + 1 <= this.cutPageCount) void this.renderCutPageCanvas(num + 1).catch(() => {});
      } catch (e) {
        if (token === this.cutRun) this.cutError = '渲染第 ' + num + ' 页失败：' + e.message;
      } finally {
        if (token === this.cutRun) this.cutPageBusy = false;
      }
    },
    async renderCutThumbnails(token) {
      if (this.cutSourceKind === 'photos') {
        const urls = {};
        for (const p of this.cutPhotoPages) urls[p.page] = p.url;
        this.cutThumbUrls = urls;
        return;
      }
      if (!this.cutPdfDoc) return;
      for (let n = 1; n <= this.cutPageCount; n++) {
        if (token !== this.cutRun || !this.cutPdfDoc) return;
        try {
          const page = await this.cutPdfDoc.getPage(n);
          const orig = page.getViewport({ scale: 1 });
          const vp = page.getViewport({ scale: 160 / Math.max(orig.width, orig.height) });
          const c = document.createElement('canvas');
          c.width = Math.ceil(vp.width);
          c.height = Math.ceil(vp.height);
          await page.render({ canvasContext: c.getContext('2d'), viewport: vp }).promise;
          if (token !== this.cutRun) return;
          this.cutThumbUrls = { ...this.cutThumbUrls, [n]: c.toDataURL('image/jpeg', 0.72) };
        } catch {}
      }
    },
    cutStagePoint(event) {
      const el = this.$refs.cutStage;
      if (!el) return { x: 0, y: 0 };
      const b = el.getBoundingClientRect();
      const clamp = v => Math.max(0, Math.min(1, v));
      return { x: clamp((event.clientX - b.left) / b.width), y: clamp((event.clientY - b.top) / b.height) };
    },
    beginCutDraw(event) {
      if (this.cutPageBusy || this.cutLoading || event.button !== 0) return;
      this.cutAnchor = this.cutStagePoint(event);
      this.cutActiveBox = null;
      event.currentTarget.setPointerCapture?.(event.pointerId);
    },
    moveCutDraw(event) {
      if (!this.cutAnchor) return;
      const a = this.cutAnchor, b = this.cutStagePoint(event);
      this.cutActiveBox = { x: Math.min(a.x, b.x), y: Math.min(a.y, b.y), w: Math.abs(a.x - b.x), h: Math.abs(a.y - b.y) };
    },
    async endCutDraw(event) {
      if (!this.cutAnchor) return;
      this.moveCutDraw(event);
      const box = this.cutActiveBox;
      const pt = this.cutStagePoint(event);
      this.cutAnchor = null;
      this.cutActiveBox = null;
      const cw = this.cutPageSize.w || 1000, ch = this.cutPageSize.h || 1400;
      if (!box || box.w * cw < 14 || box.h * ch < 14) {
        // Click hit-test: select existing box under cursor.
        const hit = [...this.cutPageRegions].reverse().find(r =>
          r.itemId && pt.x >= r.box.x && pt.x <= r.box.x + r.box.w && pt.y >= r.box.y && pt.y <= r.box.y + r.box.h
        );
        if (hit) this.cutSelectedId = hit.itemId;
        return;
      }
      const mode = (this.cutMode === 'new' && event.shiftKey && this.cutCurrentItem) ? 'append' : this.cutMode;
      const region = { page: this.cutPage, x: box.x, y: box.y, w: box.w, h: box.h };

      if (mode === 'answer') {
        this.cutAnswerRegions.push({ id: 'ans-' + selectionUUID().slice(0, 8), ...region });
        this.cutNotice = `已添加答案区（共 ${this.cutAnswerRegions.length} 块），可点「AI 识别答案区并回填」。`;
        this.queueCutDraftSave();
        return;
      }
      if (mode === 'figure') {
        const cur = this.cutCurrentItem;
        if (!cur) { this.cutNotice = '请先框选或选中一道题，再框选它的配图。'; return; }
        const fig = { id: 'fig-' + selectionUUID().slice(0, 8), ...region, enhanced: false, redrawnData: '' };
        cur.figures.push(fig);
        this.queueCutDraftSave();
        await this.refreshCutFigureThumb(fig);
        this.cutNotice = `已为第 ${cur.label} 题添加配图（图 ${cur.figures.length}）。`;
        return;
      }
      if (mode === 'append' && this.cutCurrentItem) {
        const cur = this.cutCurrentItem;
        if (cur.stored) { this.cutNotice = '该题已入库，无法继续追加选区；请新建题目。'; return; }
        cur.regions.push(region);
        this.queueCutDraftSave();
        await this.refreshCutSelectedPreview();
        if (this.cutAutoRecognize) await this.recognizeCutItem(cur, false);
        return;
      }
      // Create new cut item
      const savedNums = this.qs.flatMap(q => this.sourcesOf(q)).filter(([id]) => id === this.cutPaperId).map(([, n]) => n);
      const label = nextCutQuestionLabel(this.cutItems, savedNums);
      const item = {
        id: 'cut-' + selectionUUID().slice(0, 8),
        label,
        type: '单选题',
        source: '',
        regions: [region],
        figures: [],
        text: '',
        textMode: '',
        hasFigureHint: false,
        answer: '',
        analysis: '',
        aiDraft: null,
        aiNote: '',
        matchId: '',
        matchLabel: '',
        matchNote: '',
        link: false,
        stored: false
      };
      this.cutItems.push(item);
      this.cutSelectedId = item.id;
      this.queueCutDraftSave();
      await this.refreshCutSelectedPreview();
      if (this.cutAutoRecognize) await this.recognizeCutItem(item, false);
    },
    async stitchRegionsToDataUrl(regions, maxW = 1800, maxH = 3600) {
      if (!regions?.length) return '';
      const crops = [];
      for (const r of regions) {
        const pageCanvas = await this.renderCutPageCanvas(r.page);
        const sx = Math.max(0, Math.round(r.x * pageCanvas.width));
        const sy = Math.max(0, Math.round(r.y * pageCanvas.height));
        const sw = Math.max(4, Math.min(pageCanvas.width - sx, Math.round(r.w * pageCanvas.width)));
        const sh = Math.max(4, Math.min(pageCanvas.height - sy, Math.round(r.h * pageCanvas.height)));
        crops.push({ canvas: pageCanvas, sx, sy, sw, sh });
      }
      const gap = 12;
      const rawW = Math.max(...crops.map(c => c.sw));
      const rawH = crops.reduce((s, c) => s + c.sh, 0) + gap * (crops.length - 1);
      const scale = Math.min(1, maxW / rawW, maxH / Math.max(1, rawH));
      const out = document.createElement('canvas');
      out.width = Math.max(8, Math.round(rawW * scale));
      out.height = Math.max(8, Math.round(rawH * scale));
      const ctx = out.getContext('2d');
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, out.width, out.height);
      let y = 0;
      for (const c of crops) {
        const dw = Math.round(c.sw * scale), dh = Math.round(c.sh * scale);
        ctx.drawImage(c.canvas, c.sx, c.sy, c.sw, c.sh, 0, y, dw, dh);
        y += dh + Math.round(gap * scale);
      }
      return out.toDataURL('image/jpeg', 0.92);
    },
    async refreshCutSelectedPreview() {
      const item = this.cutCurrentItem;
      if (!item || !item.regions.length) { this.cutPreviewUrl = ''; return; }
      const cacheKey = item.id + ':' + JSON.stringify(item.regions);
      if (cutterStitchCache.has(cacheKey)) {
        this.cutPreviewUrl = cutterStitchCache.get(cacheKey);
      } else {
        try {
          const url = await this.stitchRegionsToDataUrl(item.regions, 1400, 2600);
          cutterStitchCache.set(cacheKey, url);
          if (this.cutSelectedId === item.id) this.cutPreviewUrl = url;
        } catch {
          if (this.cutSelectedId === item.id) this.cutPreviewUrl = '';
        }
      }
      for (const fig of item.figures || []) {
        if (!this.cutFigureThumbs[fig.id]) void this.refreshCutFigureThumb(fig);
      }
    },
    async renderCutFigureDataUrl(fig) {
      if (fig.redrawnData) return fig.redrawnData;
      const pageCanvas = await this.renderCutPageCanvas(fig.page);
      const sx = Math.max(0, Math.round(fig.x * pageCanvas.width));
      const sy = Math.max(0, Math.round(fig.y * pageCanvas.height));
      const sw = Math.max(4, Math.min(pageCanvas.width - sx, Math.round(fig.w * pageCanvas.width)));
      const sh = Math.max(4, Math.min(pageCanvas.height - sy, Math.round(fig.h * pageCanvas.height)));
      const scale = Math.min(1, 1800 / Math.max(sw, sh));
      const canvas = document.createElement('canvas');
      canvas.width = Math.max(4, Math.round(sw * scale));
      canvas.height = Math.max(4, Math.round(sh * scale));
      const ctx = canvas.getContext('2d');
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, canvas.width, canvas.height);
      ctx.drawImage(pageCanvas, sx, sy, sw, sh, 0, 0, canvas.width, canvas.height);
      return fig.enhanced ? enhanceFigure(canvas, 4) : canvas.toDataURL('image/png');
    },
    async refreshCutFigureThumb(fig) {
      try {
        const url = await this.renderCutFigureDataUrl(fig);
        this.cutFigureThumbs = { ...this.cutFigureThumbs, [fig.id]: url };
      } catch {}
    },
    async toggleCutFigureEnhance(fig) {
      fig.redrawnData = '';
      fig.enhanced = !fig.enhanced;
      await this.refreshCutFigureThumb(fig);
      this.queueCutDraftSave();
    },
    async openCutFigureTool(fig) {
      this.cutFigureTarget = fig;
      // Pass raw un-enhanced crop so FigureTool operates on original pixels.
      const savedEnhanced = fig.enhanced, savedRedrawn = fig.redrawnData;
      fig.enhanced = false; fig.redrawnData = '';
      try {
        this.figureSource = await this.renderCutFigureDataUrl(fig);
      } finally {
        fig.enhanced = savedEnhanced; fig.redrawnData = savedRedrawn;
      }
      this.$refs.figureTool?.showModal();
    },
    applyCutFigureTool(src) {
      const fig = this.cutFigureTarget;
      if (!fig) return;
      fig.redrawnData = src;
      fig.enhanced = false;
      this.cutFigureThumbs = { ...this.cutFigureThumbs, [fig.id]: src };
      this.cutFigureTarget = null;
      this.$refs.figureTool?.close();
      this.queueCutDraftSave();
      this.cutNotice = '配图处理结果已应用到当前题目。';
    },
    removeCutFigure(item, idx) {
      const [removed] = item.figures.splice(idx, 1);
      if (removed) {
        const copy = { ...this.cutFigureThumbs };
        delete copy[removed.id];
        this.cutFigureThumbs = copy;
      }
      this.queueCutDraftSave();
    },
    removeCutRegion(item, idx) {
      item.regions.splice(idx, 1);
      this.queueCutDraftSave();
      void this.refreshCutSelectedPreview();
    },
    deleteCutItem(item) {
      if (item.stored && !confirm(`第 ${item.label} 题已入库，仅从本切题列表移除（不删题库里已收录的题）？`)) return;
      this.cutItems = this.cutItems.filter(it => it.id !== item.id);
      if (this.cutSelectedId === item.id) {
        this.cutSelectedId = (this.cutItems.find(it => !it.stored) || this.cutItems[0])?.id || '';
      }
      this.queueCutDraftSave();
    },
    async recognizeCutItem(item, forceVision = false) {
      if (!item || !item.regions.length) return;
      this.cutBusy = true;
      this.cutError = '';
      if (!this.cutBatchBusy) this.cutBatchNotice = '';
      try {
        // 1. Digital PDF text layer fast path when not forcing vision.
        if (!forceVision && this.cutSourceKind === 'pdf' && this.cutPdfDoc) {
          const segTexts = [];
          for (const r of item.regions) {
            const page = await this.cutPdfDoc.getPage(r.page);
            const vp = page.getViewport({ scale: 1 });
            const tc = await page.getTextContent();
            segTexts.push(extractPdfRegionText(tc.items, vp, r));
          }
          const combined = segTexts.filter(Boolean).join('\n');
          if (isUsablePdfText(combined)) {
            item.text = sciText(combined);
            item.textMode = 'text-layer';
            item.source = item.source || printedQuestionSource(item.text);
            item.type = this.guessCutType(item.text, item.type);
            const numMatch = item.text.match(/^\s*(?:(例\s*\d+)|(\d+))\s*[.．、]/);
            if (numMatch) item.label = normalizeCutQuestionNo(numMatch[1] || numMatch[2]);
            await this.checkCutDuplicate(item);
            this.queueCutDraftSave();
            this.cutNotice = `第 ${item.label} 题已直接读取 PDF 文本层（0 费用）。`;
            return;
          }
        }
        // 2. Vision model path.
        if (!this.aiReady) {
          item.textMode = 'manual';
          this.cutNotice = '未配置 AI，请对照右侧原图直接编辑题面，或配置 AI 后点击「视觉识别」。';
          return;
        }
        this.cutNotice = `正在用视觉模型识别第 ${item.label} 题…`;
        const stitched = await this.stitchRegionsToDataUrl(item.regions);
        const prompt = [
          '精准转写图片中的这一道高中生物题（可能包含上下两栏或跨页拼接段落）。',
          '要求：',
          '1. 保留题号、选项（A/B/C/D）、小问号、单位和化学/遗传上下标，上下标直接用 Unicode 书写（如 ¹⁵N、CO₂、H₂O、Ca²⁺）。',
          '2. 保留题号附近印刷的年份、地区或学校来源到 source（必须在 text 中找得到，没有则留空），忽略页眉、手写答案和批改痕迹。',
          '3. 不解题、不补写缺字，模糊处写〔待核〕。',
          '4. 判断题型为：单选题、多选题、判断题、实验题、填空题、非选择题 之一。',
          '5. 若题内包含必要的生物示意图、曲线图或表格，设 hasFigure 为 true。',
          '只返回 JSON：{"number":"原题号","type":"题型","text":"完整题面文字","source":"印刷来源","hasFigure":false}。'
        ].join('\n');
        const reply = await requestQuestionAI(this.aiConnection, prompt, [stitched]);
        const parsed = parseVisionCutResponse(reply);
        item.text = parsed.text;
        item.textMode = 'vision';
        if (parsed.type) item.type = parsed.type;
        else item.type = this.guessCutType(item.text, item.type);
        if (parsed.number) item.label = parsed.number;
        if (parsed.source) item.source = parsed.source;
        item.hasFigureHint = parsed.hasFigure;
        await this.checkCutDuplicate(item);
        this.queueCutDraftSave();
        this.cutNotice = `第 ${item.label} 题视觉识别完成，请校对后确认入库。`;
      } catch (e) {
        this.cutNotice = '识别失败：' + e.message;
      } finally {
        this.cutBusy = false;
      }
    },
    guessCutType(text, fallback) {
      if (/[A-D]\s*[.．、]/.test(text)) return /多选|多项/.test(text) ? '多选题' : '单选题';
      if (/实验|对照组|实验组|自变量|因变量|探究/.test(text)) return '实验题';
      return fallback || '非选择题';
    },
    async batchRecognizeCutItems() {
      if (this.cutBatchBusy) { this.cutRun++; this.cutBatchBusy = false; this.cutError = ''; this.cutBatchNotice = '已停止批量识别。'; return; }
      this.cutError = '';
      this.cutBatchNotice = '';
      const targets = this.cutItems.filter(it => !it.stored && !it.text.trim() && it.regions.length);
      if (!targets.length) { this.cutBatchNotice = '没有待识别的空白题框。'; return; }
      this.cutBatchBusy = true;
      const run = this.cutRun;
      for (const [i, it] of targets.entries()) {
        if (run !== this.cutRun) break;
        this.cutBatchNotice = `批量识别中 ${i + 1} / ${targets.length}（第 ${it.label} 题）…`;
        this.cutSelectedId = it.id;
        await this.recognizeCutItem(it, false);
      }
      if (run === this.cutRun) {
        this.cutBatchBusy = false;
        this.cutBatchNotice = `已完成 ${targets.length} 题识别，请逐题核对。`;
      }
    },
    async recognizeAnswerRegions() {
      this.cutError = '';
      this.cutBatchNotice = '';
      if (!this.cutAnswerRegions.length) { this.cutNotice = '请先切换到「框选答案区」并在答案页拖出选区。'; return; }
      if (!this.aiReady) { this.$refs.intakeAI?.showModal(); return; }
      this.cutAnswerBusy = true;
      this.cutNotice = '正在识别参考答案区并按题号回填…';
      try {
        const stitched = await this.stitchRegionsToDataUrl(this.cutAnswerRegions, 1800, 4200);
        const prompt = [
          '识别这张高中生物试卷参考答案与评分标准截图中的所有题目答案和解析。',
          '选择题答案只写选项字母（如 B 或 ABD），判断题只写 √ 或 ×，上下标用 Unicode（如 ¹⁵N、CO₂、Ca²⁺）。',
          '没有印刷解析的题 analysis 留空字符串，不要编造。',
          '只返回 JSON：{"answers":[{"number":"题号","answer":"答案","analysis":"解析"}]}。'
        ].join('\n');
        const reply = await requestQuestionAI(this.aiConnection, prompt, [stitched]);
        const rows = parseAnswerRegionJson(reply);
        if (!rows.length) throw Error('未能从选区中解析出带题号的参考答案。');
        const byNo = new Map();
        for (const it of this.cutItems) {
          if (!it.stored) byNo.set(normalizeCutQuestionNo(it.label), it);
        }
        let filled = 0, unmatched = [];
        for (const r of rows) {
          const target = byNo.get(r.number);
          if (!target) { unmatched.push(r.number); continue; }
          let touched = false;
          if (r.answer && !target.answer.trim()) { target.answer = r.answer; touched = true; }
          if (r.analysis && !target.analysis.trim()) { target.analysis = r.analysis; touched = true; }
          if (touched) filled++;
        }
        this.queueCutDraftSave();
        this.cutNotice = `已从答案区识别 ${rows.length} 题，自动回填 ${filled} 题` +
          (unmatched.length ? `；未匹配题号（尚未切题）：${unmatched.slice(0, 8).join('、')}` : '。');
      } catch (e) {
        this.cutNotice = '答案区识别失败：' + e.message;
      } finally {
        this.cutAnswerBusy = false;
      }
    },
    async draftCutItemSolution(item) {
      this.cutError = '';
      if (!this.cutBatchBusy) this.cutBatchNotice = '';
      if (!item || !item.text.trim()) { this.cutNotice = '请先识别或填写题面文字。'; return; }
      if (!this.aiReady) { this.$refs.intakeAI?.showModal(); return; }
      this.cutBusy = true;
      this.cutNotice = `AI 正在为第 ${item.label} 题起草答案与解析…`;
      try {
        const needAnswer = !item.answer.trim(), needAnalysis = !item.analysis.trim();
        if (!needAnswer && !needAnalysis) { this.cutNotice = '答案和解析均已填写。'; return; }
        const imgs = [];
        for (const f of item.figures) imgs.push(await this.renderCutFigureDataUrl(f));
        if (!imgs.length && item.regions.length) imgs.push(await this.stitchRegionsToDataUrl(item.regions, 1400, 2200));
        const prompt = [
          `请为下面这道高中生物${item.type}起草${needAnswer && needAnalysis ? '答案和解析' : needAnswer ? '答案' : '解析'}，供教师审核。`,
          item.type === '判断题' ? '答案只写 √ 或 ×。' : /选/.test(item.type) ? '答案只写选项字母。' : '答案按原题小问顺序作答。',
          needAnalysis ? '解析结合题干说明判断依据，简明扼要，符合人教版高中生物。' : '',
          '上下标用 Unicode（¹⁵N、CO₂、Ca²⁺）。缺图或条件不足请在 note 中说明，不要猜测。',
          '只返回 JSON：{"answer":"","analysis":"","note":""}。',
          '题面：\n' + item.text + (needAnswer ? '' : '\n参考答案：' + item.answer)
        ].filter(Boolean).join('\n');
        const reply = await requestQuestionAI(this.aiConnection, prompt, imgs);
        const data = JSON.parse(reply.trim().replace(/^```(?:json)?\s*|\s*```$/gi, ''));
        const f = v => typeof v === 'string' ? v.trim() : '';
        const draft = { answer: needAnswer ? sciText(f(data.answer)) : '', analysis: needAnalysis ? sciText(f(data.analysis)) : '' };
        if (!draft.answer && !draft.analysis) throw Error(f(data.note) || '未生成可用草稿');
        item.aiDraft = draft;
        item.aiNote = f(data.note) ? 'AI 提示：' + f(data.note) : '';
        this.queueCutDraftSave();
        this.cutNotice = `第 ${item.label} 题 AI 草稿已就绪，请核对后点「采用草稿」。`;
      } catch (e) {
        this.cutNotice = 'AI 起草失败：' + e.message;
      } finally {
        this.cutBusy = false;
      }
    },
    async batchDraftCutSolutions() {
      if (this.cutBatchBusy) { this.cutRun++; this.cutBatchBusy = false; this.cutError = ''; this.cutBatchNotice = '已停止批量起草。'; return; }
      this.cutError = '';
      this.cutBatchNotice = '';
      if (!this.aiReady) { this.$refs.intakeAI?.showModal(); return; }
      const targets = this.cutItems.filter(it => !it.stored && it.text.trim() && !solutionReady({ answer: it.answer, analysis: it.analysis }) && !it.aiDraft);
      if (!targets.length) { this.cutBatchNotice = '没有需要起草答案或解析的题目。'; return; }
      this.cutBatchBusy = true;
      const run = this.cutRun;
      for (const [i, it] of targets.entries()) {
        if (run !== this.cutRun) break;
        this.cutBatchNotice = `AI 批量起草 ${i + 1} / ${targets.length}（第 ${it.label} 题）…`;
        await this.draftCutItemSolution(it);
      }
      if (run === this.cutRun) {
        this.cutBatchBusy = false;
        this.cutBatchNotice = `已为 ${targets.length} 题起草完毕，请逐题审核采纳。`;
      }
    },
    adoptCutDraft(item) {
      if (!item?.aiDraft) return;
      if (item.aiDraft.answer) item.answer = item.aiDraft.answer;
      if (item.aiDraft.analysis) item.analysis = item.aiDraft.analysis;
      item.aiDraft = null;
      item.aiNote = '';
      this.queueCutDraftSave();
    },
    async checkCutDuplicate(item) {
      if (!item?.text?.trim()) return;
      try {
        const html = plainBlocks(item.text);
        const match = await this.findDuplicate({ contentHtml: html, stem: item.text.split('\n')[0] });
        if (match) {
          item.matchId = match.q.id;
          item.matchLabel = this.matchSource(match.q);
          item.matchNote = this.dupNote(match);
        } else {
          item.matchId = item.matchLabel = item.matchNote = '';
          item.link = false;
        }
      } catch {}
    },
    async storeCutItem(item) {
      if (!item || item.stored) return false;
      const label = normalizeCutQuestionNo(item.label);
      if (!label) throw Error('请填写原题号。');
      if (!item.text.trim()) throw Error('请先识别或填写题面文字。');
      if (!solutionReady({ answer: item.answer, analysis: item.analysis })) throw Error('请补齐答案和解析后再入库。');
      if (this.qs.some(row => this.sourcesOf(row).some(([pid, n]) => pid === this.cutPaperId && normalizeCutQuestionNo(n) === label))) {
        throw Error('本卷已收录第 ' + label + ' 题，请修改题号后再入库。');
      }
      item.label = label;

      let contentHtml = plainBlocks(item.text);
      for (const fig of item.figures || []) {
        const src = await this.renderCutFigureDataUrl(fig);
        contentHtml += '<p><img alt="题目配图" src="' + src + '"></p>';
      }
      const referencePhoto = item.regions.length ? await this.stitchRegionsToDataUrl(item.regions, 1200, 2000) : '';
      await this.checkCutDuplicate(item);

      const allExisting = [...(this.qs || []), ...(this.linkedRows || []).map(r => r.q).filter(Boolean)];
      const q = buildCutQuestionRecord(this.cutPaperId, this.cutPaperOrigin, item, contentHtml, referencePhoto, allExisting);
      const link = !!(item.link && item.matchId);
      const record = { id: q.id, questions: link ? [] : [q], links: link ? [{ q, target: item.matchId }] : [] };
      const inserted = await this.addImportRecord(record);
      item.stored = true;
      if (inserted && !link) void this.classifyQuestions([q]);
      return true;
    },
    async confirmCutItem(item) {
      if (this.cutBusy) return;
      this.cutError = '';
      this.cutBatchNotice = '';
      this.cutBusy = true;
      try {
        await this.storeCutItem(item);
        await this.saveCutterDraft(true);
        void this.autoUploadPaper?.(this.cutPaperId, { origin: this.cutPaperOrigin, sourceKind: this.cutSourceKind });
        this.cutNotice = `第 ${item.label} 题已收录进题库！`;
        this.notify(this.cutNotice);
        const next = this.cutItems.find(it => !it.stored);
        if (next) this.cutSelectedId = next.id;
      } catch (e) {
        this.cutNotice = '入库失败：' + e.message;
      } finally {
        this.cutBusy = false;
      }
    },
    async storeAllReadyCutItems() {
      const ready = this.cutItems.filter(it => !it.stored && it.text.trim() && solutionReady({ answer: it.answer, analysis: it.analysis }));
      if (!ready.length || this.cutBusy) return;
      this.cutError = '';
      this.cutBatchNotice = '';
      this.cutBusy = true;
      let ok = 0;
      try {
        for (const it of ready) {
          await this.storeCutItem(it);
          ok++;
        }
        this.cutNotice = `已批量入库 ${ok} 道题。`;
        this.notify(this.cutNotice);
      } catch (e) {
        this.cutNotice = `已入库 ${ok} 题，遇到问题：${e.message}`;
      } finally {
        if (ok > 0) {
          try { await this.saveCutterDraft(true); } catch (draftErr) { console.warn('[biofolio] saveCutterDraft after partial batch error:', draftErr?.message || draftErr); }
          void this.autoUploadPaper?.(this.cutPaperId, { origin: this.cutPaperOrigin, sourceKind: this.cutSourceKind });
        }
        this.cutBusy = false;
      }
    }
  }
};
