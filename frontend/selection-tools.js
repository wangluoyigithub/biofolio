// Local-only controls for the selection preview.
function byCollectionTime(a,b) {
  return String(b.collectedAt || b.date).localeCompare(String(a.collectedAt || a.date));
}
// An answer of just √ or × marks a true/false item whatever its stored type.
function looksJudgment(q) {
  const answer = String(q.answer || q.answerHtml || '').replace(/<[^>]*>/g,'').replace(/&nbsp;/g,'').trim();
  return /^[√×✓✗✔✘对错]$/.test(answer);
}
function primaryType(question) {
  if (looksJudgment(question)) return '判断题';
  if (['单选题','单项选择题','选择题'].includes(question.type)) return '单选题';
  if (['多选题','多项选择题'].includes(question.type)) return '多选题';
  if (question.category) return question.category;
  if (['非选择题','解答题','简答题'].includes(question.type)) return openType(question);
  return question.type;
}
// Imported non-choice items carry no finer type; read the face to file them as 实验题 or 填空题.
function openType(q) {
  const text = String(q.body || '') + String(q.stem || '') + String(q.contentHtml || '').replace(/<[^>]*>/g,'');
  return /实验|对照组|实验组|自变量|因变量|无关变量|预期结果|实验思路|实验步骤|探究/.test(text) ? '实验题' : '填空题';
}

const basketSections = ['判断题','单项选择题','多项选择题','非选择题'];
function basketSection(q) {
  if (q.type === '判断题' || looksJudgment(q)) return 0;
  if (['单选题','单项选择题','选择题'].includes(q.type)) return 1;
  if (['多选题','多项选择题'].includes(q.type)) return 2;
  return 3;
}

// Keep exact evidence of a shared figure or a substantial introductory passage.
async function materialKeys(q) {
  const keys = new Set();
  for (const src of aiQuestionParts(q).images) {
    keys.add('图:' + await pictureKey(src));
  }
  const intro = (q.stem || '').normalize('NFKC').replace(/^\s*(?:\d+[.、]\s*)?(?:\(\d+分\)\s*)?(?:\((?:19|20)\d{2}[^)]*\)\s*)?/, '');
  for (const sentence of intro.split(/[。！？\n]/)) {
    const passage = sentence.replace(/\s/g,'').split(/下列|据此|据图回答|请回答|请分析/)[0];
    if (passage.length >= 24 && !/^[A-D][.、]|正确的是|错误的是|符合题意/.test(passage)) keys.add('文:' + passage);
  }
  return keys;
}

const scoreMetaRowRe = /^(?:满分|标准分|赋分|标准答案|参考答案|正确答案|答案|平均分|均分|班级均分|年级均分|校均分|得分率|正确率|错误率|失分率|难度|区分度|标准差|最高分|最低分|合计|总计|汇总|统计|实考人数|参考人数|作答人数|应考人数|缺考人数)$/;
const scoreMaxLabelRe = /^(?:满分|标准分|赋分)$/;
const scoreAnsLabelRe = /^(?:标准答案|参考答案|正确答案|答案)$/;
const scoreStudentColRe = /^(?:学生|姓名|学生姓名|人名|名字|考生|学号|考号|准考证号|自定义考号|班级|行政班|教学班|序号|座位号)$/;
const scoreSkipValRe = /^(?:缺考|免考|未考)$/;
const scoreUnansweredRe = /^(?:\?|？|-|--|—|\/|未选|未涂|未答|空|无)$/;

function decodeScoreCsvBytes(bytes) {
  let text;
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    text = new TextDecoder('gb18030').decode(bytes);
  }
  return text.replace(/^\uFEFF/, '');
}

function normalizeChoiceAnswer(raw) {
  const text = String(raw ?? '').replace(/<[^>]*>/g, '').normalize('NFKC').trim()
    .replace(/^(?:标准|参考|正确)?答案[:：\s]*/, '').replace(/[,，、.．;；\s]/g, '').toUpperCase();
  if (!text || !/^[A-H]+$/.test(text)) return '';
  return [...new Set(text)].sort().join('');
}

function normalizeJudgmentAnswer(raw) {
  const text = String(raw ?? '').replace(/<[^>]*>/g, '').normalize('NFKC').trim()
    .replace(/^(?:标准|参考|正确)?答案[:：\s]*/, '').toUpperCase();
  if (['√', '✓', '✔', '对', '正确', 'T', 'TRUE', '是'].includes(text)) return 'T';
  if (['×', '✗', '✘', '错', '错误', 'F', 'FALSE', '否'].includes(text)) return 'F';
  return '';
}

function buildScoreSpec(edition, getQuestion, globalFixes, maxOverrides = {}) {
  const ids = edition?.ids || [];
  const grading = edition?.grading || null;
  const items = {};
  for (let idx = 0; idx < ids.length; idx++) {
    const id = ids[idx], n = idx + 1;
    const q = getQuestion ? getQuestion(id, edition) : null;
    const rule = grading?.items?.[id];
    const kind = rule ? rule.kind : (q ? basketSection(q) : 3);
    const typeName = ['判断题', '单选题', '多选题', '非选择题'][kind] || '题目';
    const rawAns = String(edition?.fixes?.[id]?.answer || globalFixes?.[id]?.answer || (q ? answerText(q) : '') || '').trim();
    const answer = kind === 0 ? normalizeJudgmentAnswer(rawAns)
      : (kind === 1 || kind === 2) ? normalizeChoiceAnswer(rawAns) : '';
    const stemPts = Number(String(q?.stem || '').match(/[（(]\s*(?:共|本题)?\s*(\d+(?:\.\d+)?)\s*分\s*[）)]/)?.[1]) || null;
    const gradingMax = rule?.points > 0 ? Number(rule.points) : null;
    const defaultMax = kind === 0 ? (Number(grading?.judgment) || 1)
      : kind === 1 ? (Number(grading?.single) || 2)
      : kind === 2 ? (Number(grading?.multi) || 3)
      : stemPts;
    items[n] = {
      id, kind, typeName, answer, gradingMax,
      max: gradingMax ?? defaultMax,
      partial: kind === 2 ? (grading?.partial ?? 1) : 0
    };
  }
  return {
    totalTarget: grading?.total > 0 ? Number(grading.total) : null,
    maxOverrides: { ...maxOverrides },
    items
  };
}

function matchScoreHeader(cells, length, initialHead = null) {
  const qCol = cells.indexOf('题号'), errCol = cells.indexOf('错误人数'), totalCol = cells.indexOf('作答人数');
  if (qCol >= 0 && errCol >= 0) return { mode: 'summary', qCol, errCol, totalCol };
  const listCol = cells.indexOf('错题号');
  if (listCol >= 0) return { mode: 'wrongList', listCol };

  const firstNonEmpty = cells.find(c => c !== '') || '';
  if (scoreMetaRowRe.test(firstNonEmpty)) return null;

  const explicitCols = [];
  for (let i = 0; i < cells.length; i++) {
    const m = /^(?:第\s*(\d+)\s*题|Q\s*(\d+))(?:\s*[（(]\s*(\d+(?:\.\d+)?)\s*分?\s*[)）])?$/i.exec(cells[i]);
    if (!m) continue;
    const n = Number(m[1] ?? m[2]);
    if (!Number.isInteger(n) || n < 1 || n > length) {
      throw Error('题号超出本卷范围，请核对最终印发顺序。');
    }
    explicitCols.push({ i, n, headerMax: m[3] ? Number(m[3]) : null });
  }
  if (explicitCols.length) {
    if (new Set(explicitCols.map(c => c.n)).size !== explicitCols.length) {
      throw Error('得分表有重复题号。');
    }
    return { mode: 'detail', cols: explicitCols, explicit: true };
  }

  const hasStudentLabel = cells.some(c => scoreStudentColRe.test(c));
  const bareCols = [];
  let badBare = false;
  let badReason = '';
  let hasLargeNum = false;
  for (let i = 0; i < cells.length; i++) {
    const v = cells[i];
    if (v === '') continue;
    if (/^-?\d+(?:\.\d+)?$/.test(v)) {
      const num = Number(v);
      if (num > 999) hasLargeNum = true;
      if (!Number.isInteger(num) || num < 1 || num > length) {
        badBare = true;
        if (Number.isInteger(num) && (num < 1 || num > length)) badReason = 'range';
      } else {
        bareCols.push({ i, n: num, headerMax: null });
      }
      continue;
    }
    const m = /^(\d+)\s*[（(]\s*(\d+(?:\.\d+)?)\s*分?\s*[)）]$/.exec(v);
    if (m) {
      const n = Number(m[1]);
      if (n < 1 || n > length) { badBare = true; badReason = 'range'; }
      else bareCols.push({ i, n, headerMax: Number(m[2]) });
    }
  }

  if (hasStudentLabel) {
    if (badBare) {
      if (badReason === 'range' && !hasLargeNum && new Set(bareCols.map(c => c.n)).size === bareCols.length) {
        throw Error('题号超出本卷范围，请核对最终印发顺序。');
      }
      return null;
    }
    if (bareCols.length) {
      if (new Set(bareCols.map(c => c.n)).size !== bareCols.length) {
        throw Error('得分表有重复题号。');
      }
      return { mode: 'detail', cols: bareCols, explicit: false };
    }
    return null;
  }

  if (initialHead) {
    if (cells.join('\0') === initialHead.join('\0') && !badBare && bareCols.length) {
      return { mode: 'detail', cols: bareCols, explicit: false };
    }
    return null;
  }

  if (!badBare && bareCols.length && new Set(bareCols.map(c => c.n)).size === bareCols.length &&
      cells[0] !== '' && isNaN(Number(cells[0])) &&
      (bareCols.length === length || (bareCols[0].n === 1 && bareCols.every((c, idx) => idx === 0 || c.n > bareCols[idx - 1].n)))) {
    return { mode: 'detail', cols: bareCols, explicit: false };
  }
  return null;
}

function parseScoreBlock(rows, length) {
  const clean = rows
    .map(row => row.map(value => String(value ?? '').trim()))
    .filter(row => row.some(v => v !== ''));
  if (clean.length < 2) throw Error('表格需要表头和至少一行数据。');

  let headIdx = -1;
  let header = null;
  for (let r = 0; r < clean.length; r++) {
    header = matchScoreHeader(clean[r], length);
    if (header) { headIdx = r; break; }
  }
  if (!header) throw Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');

  const head = clean[headIdx];
  const headKey = head.join('\0');
  const body = clean.slice(headIdx + 1);
  const number = value => {
    const n = Number(value);
    if (!Number.isInteger(n) || n < 1 || n > length) throw Error('题号超出本卷范围，请核对最终印发顺序。');
    return n;
  };
  const count = value => {
    if (value == null || String(value).trim() === '' || !Number.isInteger(+value) || +value < 0) throw Error('人数须为非负整数。');
    return +value;
  };

  if (header.mode === 'summary') {
    const seen = new Set();
    const out = [];
    for (const row of body) {
      if (row.join('\0') === headKey) continue;
      const n = number(row[header.qCol]), missed = count(row[header.errCol]);
      const total = header.totalCol < 0 || String(row[header.totalCol] ?? '').trim() === '' ? null : count(row[header.totalCol]);
      if (seen.has(n) || (total !== null && missed > total)) throw Error('出现重复题号或错误人数大于作答人数。');
      seen.add(n);
      out.push({ n, missed, total });
    }
    return { mode: 'summary', rows: out };
  }

  if (header.mode === 'wrongList') {
    const counts = new Map();
    let rowCount = 0;
    for (const row of body) {
      if (row.join('\0') === headKey) continue;
      rowCount++;
      const raw = String(row[header.listCol] ?? '').trim();
      if (!raw || raw === '无') continue;
      const ids = new Set(raw.split(/[,，、;；\s]+/).filter(Boolean).map(number));
      ids.forEach(n => counts.set(n, (counts.get(n) || 0) + 1));
    }
    return { mode: 'wrongList', counts, rowCount };
  }

  let activeCols = header.cols;
  const colOrder = [];
  const colSeen = new Set();
  const headerMax = {};
  const recordCols = cols => {
    for (const { n, headerMax: hm } of cols) {
      if (!colSeen.has(n)) { colSeen.add(n); colOrder.push(n); }
      if (hm > 0) headerMax[n] = hm;
    }
  };
  recordCols(activeCols);

  const fileMaxByQ = {};
  const fileAnsByQ = {};
  const students = [];
  let hasFileMax = false;

  for (const row of body) {
    const repeated = matchScoreHeader(row, length, head);
    if (repeated?.mode === 'detail') {
      activeCols = repeated.cols;
      recordCols(activeCols);
      continue;
    }
    const minColIdx = Math.min(...activeCols.map(c => c.i));
    const colIdxSet = new Set(activeCols.map(c => c.i));
    const metaCells = minColIdx > 0 ? row.slice(0, minColIdx) : row.filter((_, i) => !colIdxSet.has(i));
    if (metaCells.some(c => scoreMaxLabelRe.test(c))) {
      for (const { i, n } of activeCols) {
        const max = Number(row[i]);
        if (!Number.isFinite(max) || max <= 0) throw Error('每道题需要正数满分。');
        fileMaxByQ[n] = max;
      }
      hasFileMax = true;
      continue;
    }
    if (metaCells.some(c => scoreAnsLabelRe.test(c))) {
      for (const { i, n } of activeCols) {
        if (row[i]) fileAnsByQ[n] = row[i];
      }
      continue;
    }
    if (metaCells.some(c => scoreMetaRowRe.test(c) || scoreSkipValRe.test(c))) continue;
    if (activeCols.every(({ i }) => !row[i] || scoreSkipValRe.test(row[i]))) continue;

    const student = {};
    for (const { i, n } of activeCols) student[n] = row[i] || '';
    students.push(student);
  }

  return { mode: 'detail', colOrder, headerMax, fileMaxByQ, fileAnsByQ, hasFileMax, students };
}

function evaluateScoreDatasets(blocks, length, spec = null) {
  if (!blocks.length) throw Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');
  const modes = new Set(blocks.map(b => b.mode));
  if (modes.size > 1) throw Error('请使用相同类型的成绩表合并导入。');
  const mode = blocks[0].mode;

  if (mode === 'summary') {
    const byN = new Map();
    for (const b of blocks) {
      for (const r of b.rows) {
        const cur = byN.get(r.n);
        if (!cur) {
          byN.set(r.n, { n: r.n, missed: r.missed, total: r.total });
        } else {
          cur.missed += r.missed;
          cur.total = (cur.total !== null && r.total !== null) ? cur.total + r.total : null;
        }
      }
    }
    const items = Array.from(byN.values()).map(r => ({
      ...r,
      max: spec?.items?.[r.n]?.max ?? null,
      lossRate: r.total ? Math.max(0, Math.min(1, r.missed / r.total)) : null,
      mean: null,
      kind: spec?.items?.[r.n]?.kind ?? null,
      typeName: spec?.items?.[r.n]?.typeName || ''
    }));
    return { mode, items, hasFileMax: false, autoMaxCols: [], inferredMax: {}, adaptedFromGrading: false, examineeCount: null };
  }

  if (mode === 'wrongList') {
    const counts = new Map();
    let examineeCount = 0;
    for (const b of blocks) {
      examineeCount += b.rowCount;
      for (const [n, c] of b.counts) counts.set(n, (counts.get(n) || 0) + c);
    }
    if (!counts.size) throw Error('表中没有记录错题号。');
    const items = Array.from(counts, ([n, missed]) => ({
      n, missed, total: null,
      max: spec?.items?.[n]?.max ?? null,
      lossRate: null, mean: null,
      kind: spec?.items?.[n]?.kind ?? null,
      typeName: spec?.items?.[n]?.typeName || ''
    }));
    return { mode, items, hasFileMax: false, autoMaxCols: [], inferredMax: {}, adaptedFromGrading: false, examineeCount };
  }

  const headerMax = {};
  const fileMaxByQ = {};
  const fileAnsByQ = {};
  const colOrder = [];
  const colSeen = new Set();
  const students = [];
  let hasFileMax = false;
  for (const b of blocks) {
    if (b.hasFileMax) hasFileMax = true;
    for (const n of b.colOrder) {
      if (!colSeen.has(n)) { colSeen.add(n); colOrder.push(n); }
    }
    Object.assign(headerMax, b.headerMax);
    Object.assign(fileMaxByQ, b.fileMaxByQ);
    Object.assign(fileAnsByQ, b.fileAnsByQ);
    for (const s of b.students) students.push(s);
  }
  if (!colOrder.length || (!hasFileMax && !spec)) {
    throw Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');
  }
  if (!students.length) throw Error('表中没有找到学生作答记录。');

  const hasNumeric = {};
  const observedMax = {};
  for (const n of colOrder) { hasNumeric[n] = false; observedMax[n] = 0; }
  for (const s of students) {
    for (const n of colOrder) {
      const v = s[n];
      if (!v || scoreSkipValRe.test(v)) continue;
      if (/^-?\d+(?:\.\d+)?$/.test(v)) {
        const num = Number(v);
        if (!Number.isFinite(num) || num < 0) throw Error('第 ' + n + ' 题含异常得分。');
        hasNumeric[n] = true;
        if (num > observedMax[n]) observedMax[n] = num;
      }
    }
  }

  const autoMaxCols = [];
  const inferredMax = {};
  let adaptedFromGrading = false;
  if (!hasFileMax && spec) {
    for (const n of colOrder) {
      if (hasNumeric[n] && (spec.items[n]?.kind ?? 3) === 3 && !(headerMax[n] > 0)) {
        autoMaxCols.push(n);
      }
    }
    const exceeded = autoMaxCols.some(n => spec.items[n]?.gradingMax > 0 && observedMax[n] > spec.items[n].gradingMax);
    const nonAutoSum = colOrder
      .filter(n => !autoMaxCols.includes(n))
      .reduce((sum, n) => sum + (headerMax[n] || spec.items[n]?.max || 0), 0);
    const observedAutoSum = autoMaxCols.reduce((sum, n) => sum + observedMax[n], 0);
    const totalMatchesObserved = exceeded && spec.totalTarget > 0 && colOrder.length === length &&
      autoMaxCols.every(n => observedMax[n] > 0) && (nonAutoSum + observedAutoSum === spec.totalTarget);

    for (const n of autoMaxCols) {
      const base = spec.items[n]?.max || 0;
      const candidate = totalMatchesObserved ? observedMax[n] : Math.max(base, observedMax[n]);
      if (candidate > 0) inferredMax[n] = candidate;
      if (spec.items[n]?.gradingMax > 0 && candidate !== spec.items[n].gradingMax) adaptedFromGrading = true;
    }
  }

  const attachContextError = msg => {
    const err = Error(msg);
    if (autoMaxCols.length) {
      err.autoMaxCols = autoMaxCols;
      err.inferredMax = inferredMax;
    }
    return err;
  };

  const effMax = {};
  for (const n of colOrder) {
    if (fileMaxByQ[n] !== undefined) {
      effMax[n] = fileMaxByQ[n];
      continue;
    }
    if (hasFileMax && !spec) {
      throw Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');
    }
    const overrideRaw = spec?.maxOverrides?.[n];
    if (overrideRaw !== undefined && String(overrideRaw).trim() !== '') {
      const m = Number(overrideRaw);
      if (!Number.isFinite(m) || m <= 0) throw attachContextError('第 ' + n + ' 题满分须为正数。');
      if (hasNumeric[n] && observedMax[n] > m) {
        throw attachContextError('第 ' + n + ' 题实考最高分为 ' + observedMax[n] + ' 分，满分不能设为 ' + m + ' 分。');
      }
      effMax[n] = m;
      continue;
    }
    if (headerMax[n] > 0) {
      if (hasNumeric[n] && observedMax[n] > headerMax[n]) throw attachContextError('第 ' + n + ' 题含异常得分。');
      effMax[n] = headerMax[n];
      continue;
    }
    if (autoMaxCols.includes(n)) {
      const m = inferredMax[n];
      if (!Number.isFinite(m) || m <= 0) {
        throw attachContextError('第 ' + n + ' 题缺少满分设置，请补充满分或先设置评分细则。');
      }
      effMax[n] = m;
      continue;
    }
    const base = spec?.items?.[n]?.max;
    const m = Math.max(Number(base) || (hasNumeric[n] ? 0 : 2), observedMax[n] || 0);
    if (!Number.isFinite(m) || m <= 0) {
      throw attachContextError('第 ' + n + ' 题缺少满分设置，请先设置评分细则。');
    }
    effMax[n] = m;
  }

  const items = colOrder.map(n => {
    const itemSpec = spec?.items?.[n];
    const kind = itemSpec?.kind ?? 3;
    const rawFileAns = fileAnsByQ[n];
    const fileNormAns = rawFileAns
      ? (kind === 0 ? normalizeJudgmentAnswer(rawFileAns) : normalizeChoiceAnswer(rawFileAns))
      : '';
    const refAns = itemSpec?.answer || fileNormAns || '';
    const max = effMax[n];
    const partial = Math.min(max, Math.max(0, Number(itemSpec?.partial ?? 1) || 0));
    let missed = 0, total = 0, scoreSum = 0;

    for (const s of students) {
      const v = s[n];
      if (!v || scoreSkipValRe.test(v)) continue;
      let score;
      if (/^-?\d+(?:\.\d+)?$/.test(v)) {
        score = Number(v);
        if (!Number.isFinite(score) || score < 0 || score > max) {
          throw attachContextError('第 ' + n + ' 题含异常得分。');
        }
      } else {
        const compact = v.normalize('NFKC').trim().replace(/[,，、.．;；\s]/g, '').toUpperCase();
        if (scoreUnansweredRe.test(compact) || scoreUnansweredRe.test(v.trim())) {
          score = 0;
        } else {
          const jVal = normalizeJudgmentAnswer(v);
          const cVal = /^[A-H]+$/.test(compact) ? [...new Set(compact)].sort().join('') : '';
          if (!jVal && !cVal) throw attachContextError('第 ' + n + ' 题含异常得分。');
          if (!refAns) {
            throw attachContextError('第 ' + n + ' 题记录的是作答选项（' + v + '），但本卷该题尚无标准答案，请先补齐答案。');
          }
          if (refAns === 'T' || refAns === 'F') {
            if (!jVal) throw attachContextError('第 ' + n + ' 题含异常得分。');
            score = jVal === refAns ? max : 0;
          } else {
            if (!cVal) throw attachContextError('第 ' + n + ' 题含异常得分。');
            const isMulti = kind === 2 || refAns.length > 1;
            if (cVal === refAns) score = max;
            else if (isMulti && [...cVal].every(ch => refAns.includes(ch))) score = partial;
            else score = 0;
          }
        }
      }
      total++;
      scoreSum += score;
      if (score < max) missed++;
    }
    const lossRate = total ? Math.max(0, Math.min(1, 1 - scoreSum / (total * max))) : null;
    const mean = total ? scoreSum / total : null;
    return { n, missed, total, max, lossRate, mean, kind, typeName: itemSpec?.typeName || '' };
  });

  return { mode, items, hasFileMax, autoMaxCols, inferredMax, adaptedFromGrading, examineeCount: students.length };
}

function readScoreRows(rows, length, spec = null) {
  return evaluateScoreDatasets([parseScoreBlock(rows, length)], length, spec).items;
}

function extractQuestionYear(q, origin, sources, aliases, paperTags) {
  const m1 = String(origin || '').match(/(?:19|20)\d{2}/);
  if (m1) return m1[0];
  const m2 = typeof printedQuestionSource === 'function'
    ? printedQuestionSource(q?.stem || '').match(/(?:19|20)\d{2}/)
    : null;
  if (m2) return m2[0];
  for (const [pid] of sources || []) {
    const label = aliases?.[pid] || pid || '';
    const m3 = String(label).match(/(?:19|20)\d{2}/);
    if (m3) return m3[0];
    const tagYear = String(paperTags?.[pid]?.year || '').match(/^(?:19|20)\d{2}/);
    if (tagYear) return tagYear[0];
  }
  return '';
}

const selectionTools = {
  data(){return {queuePage:1,appliedPhoto:null,photoPreview:'',photoSourceId:'',newStem:'',newAnswer:'',newAnalysis:'',wordChoices:[],wordNumber:0,fileSequence:0,readingSingle:false,newSource:'',newType:'单选题',pageNo:1,pageSize:(()=>{try{const n=Number(localStorage.getItem('biofolio-page-size'));return [10,20,50].includes(n)?n:10}catch{return 10}})(),paperName:'',editions:saved.editions||[],reviewId:'',reviewNumber:0,
    paperPurpose:'',purposeNames:saved.purposeNames||[],newPurpose:'',newExam:false,examPurposes:saved.examPurposes||['考试试卷','周考试卷'],purposeError:'',purposeFilter:'',formats:[],deletedFormats:saved.deletedFormats||[],gradingFormats:saved.gradingFormats||[],formatAliases:saved.formatAliases||{},studentIdDigits:normalizeStudentIdDigits(saved.studentIdDigits),schoolHeader:typeof saved.schoolHeader==='string'?saved.schoolHeader:'',gradingSettingsDigits:normalizeStudentIdDigits(saved.studentIdDigits),gradingSettingsHeader:typeof saved.schoolHeader==='string'?saved.schoolHeader:'',gradingSettingsNotice:'',formatRenameId:'',formatRenameText:'',formatMessage:'',formatBusy:false,wordBusy:false,wordNotice:{},editionEditTarget:null,editionEditName:'',editionEditPurpose:'',basketDocked:false,fixes:saved.fixes||{},editField:'',editValue:'',helper:'',scoreInput:'',scoreSources:[],scoreMessage:'',scoreNotice:'',scorePreview:[],scoreGaps:[],scoreAutoMaxCols:[],scoreMaxDraft:{},scoreMaxTouched:{},scoreThreshold:40,scoreMarks:[],scoreTab:false}},
  computed:{
    usedQuestionIds(){return new Set(this.liveEditions.flatMap(e=>e.ids||[]))},
    questionYears(){
      const set=new Set();
      for(const q of this.formalQuestions){const y=this.questionYearOf(q);if(y)set.add(y)}
      return [...set].sort().reverse();
    },
    hasUndatedQuestions(){return this.formalQuestions.some(q=>!this.questionYearOf(q))},
    filterPapers(){
      return this.livePapers.map(p=>({id:p,label:this.paperLabel(p),count:this.paperCounts.get(p)||0}))
        .filter(p=>p.count>0||p.id===this.source);
    },
    hasSingleQuestions(){return this.formalQuestions.some(q=>!this.sourcesOf(q).length)},
    liveFormats(){const gone=new Set(this.deletedFormats);return this.formats.filter(f=>!gone.has(f.id)).map(f=>({...f,displayName:this.formatAliases[f.id]||f.name}))},
    // Deleted questions stay loaded so saved papers can still show them; the bank hides them.
    livePapers(){const gone=new Set(this.deletedPapers.map(d=>d.id));return gone.size?this.papers.filter(p=>!gone.has(p)):this.papers},
    // Questions deleted one by one; those removed with a paper are listed under it.
    looseDeleted(){
      const withPaper=new Set(this.deletedPapers.flatMap(d=>d.questions||[]));
      const byId=new Map(this.qs.map(q=>[q.id,q]));
      return this.deletedQuestions.filter(id=>!withPaper.has(id)).map(id=>byId.get(id)).filter(Boolean);
    },
    liveQuestions(){const gone=new Set(this.deletedQuestions);return gone.size?this.qs.filter(q=>!gone.has(q.id)):this.qs},
    formalQuestions(){return this.liveQuestions.filter(q=>this.bankReady(q))},
    singleComplete(){return solutionReady({answer:this.singleWord?.answer,analysis:this.singleWord?.analysis,addAnswer:this.newAnswer,addAnalysis:this.newAnalysis})},
    singleWord(){return this.wordChoices.find(row=>String(row.n)===String(this.wordNumber))},
    pendingQuestions(){return this.liveQuestions.filter(q=>!this.bankReady(q)).sort(byCollectionTime)},
    queuePages(){return Math.max(1,Math.ceil(this.pendingQuestions.length/this.pageSize))},
    pageCount(){return Math.max(1,Math.ceil(this.matches.length/this.pageSize))},
    visibleQuestions(){const start=(this.pageNo-1)*this.pageSize;return this.matches.slice(start,start+this.pageSize)},
    purposes(){return [...new Set([...this.purposeNames,...this.liveEditions.map(e=>e.purpose).filter(Boolean)])]},
    filterPurposes(){return this.liveEditions.some(e=>!e.purpose)?[...new Set([...this.purposes,'未分类'])]:this.purposes},
    filteredEditions(){return this.liveEditions.filter(e=>!this.purposeFilter||(e.purpose||'未分类')===this.purposeFilter)},
    review(){return this.editions.find(p=>p.id===this.reviewId)},
    filterKey(){return JSON.stringify([this.page,this.mode,this.bookIndex,this.search,this.node,this.type,this.order,this.source,this.yearFilter,this.usageFilter,this.flags])}
  },
  watch:{photo(file){this.appliedPhoto=null;if(this.photoPreview)URL.revokeObjectURL(this.photoPreview);this.photoPreview=file?URL.createObjectURL(file):''},queuePages(n){this.queuePage=Math.min(n,this.queuePage)},filterKey(){this.pageNo=1},pageCount(n){this.pageNo=Math.min(n,this.pageNo)},reviewId(){this.resetScores();this.scoreTab=false},studentIdDigits(v){this.gradingSettingsDigits=normalizeStudentIdDigits(v)},schoolHeader(v){this.gradingSettingsHeader=typeof v==='string'?v:''}},
  methods:{
    originOf(q){return this.fixes[q.id]?.source ?? q.origin ?? ''},
    questionYearOf(q){return extractQuestionYear(q,this.originOf(q),this.sourcesOf(q),this.aliases,this.paperTags)},
    teachingNoteOf(q,edition=null){const id=typeof q==='string'?q:q?.id;return (edition?.fixes?.[id]?.teachingNote ?? this.fixes[id]?.teachingNote ?? '').trim()},
    startQuestion(paper=''){this.photoSourceId='';this.clearPdf();this.fileSequence++;this.readingSingle=false;this.wordChoices=[];this.wordNumber=0;this.singleMatch=null;this.newStem=this.newAnswer=this.newAnalysis='';this.newSource='';this.newType='单选题';this.photo=null;this.singleError='';this.singlePaper=paper;this.singleNo='';this.$refs.collector.querySelector('form').reset();this.$refs.collector.showModal()},
    async collectFromPhoto(q){
      const src=aiQuestionParts(q).images[0];
      if(!src){this.notify('未找到原照片');return}
      this.startQuestion();this.photoSourceId=q.id;this.readingSingle=true;
      const seq=this.fileSequence;
      try{
        const blob=await (await readAsset(src)).blob();
        if(seq===this.fileSequence)this.photo=new File([blob],q.stem,{type:blob.type});
      }catch(e){if(seq===this.fileSequence)this.singleError='读取原照片失败：'+e.message}
      finally{if(seq===this.fileSequence)this.readingSingle=false}
    },
    escapeFace(text){return '<p>'+escapeDoc(text)+'</p>'},
    solutionText(html,addition){
      if(!hasSolution(html))return (addition||'').trim();
      return aiQuestionParts({contentHtml:html}).text||'已有配图';
    },
    applyImportSolution(row,draft){if(draft.answer)row.addAnswer=draft.answer;if(draft.analysis)row.addAnalysis=draft.analysis},
    applySingleSolution(draft){if(draft.answer)this.newAnswer=draft.answer;if(draft.analysis)this.newAnalysis=draft.analysis},
    applyNewPhotoText(draft){if(this.newStem!==draft.text)this.newAnswer=this.newAnalysis='';this.newStem=draft.text;this.newSource=draft.source?.trim()||printedQuestionSource(draft.text);this.appliedPhoto=draft;this.singleMatch=null},
    async takeHomePhoto(event){
      const file=event.target.files[0];event.target.value='';if(!file)return;
      this.startQuestion();
      const seq=++this.fileSequence;
      if(['image/jpeg','image/png','image/webp'].includes(file.type)){
        this.photo=file;return;
      }
      this.readingSingle=true;
      const url=URL.createObjectURL(file);
      try{
        const image=new Image();image.src=url;await image.decode();
        const canvas=document.createElement('canvas');canvas.width=image.naturalWidth;canvas.height=image.naturalHeight;
        canvas.getContext('2d').drawImage(image,0,0);
        const jpeg=await new Promise(resolve=>canvas.toBlob(resolve,'image/jpeg',0.94));
        if(!jpeg)throw Error('照片转换失败');
        if(seq===this.fileSequence)this.photo=new File([jpeg],file.name.replace(/\.[^.]+$/,'')+'.jpg',{type:'image/jpeg'});
      }catch{if(seq===this.fileSequence)this.singleError='浏览器无法读取这张照片，请用 JPEG、PNG 或 WebP 格式。'}
      finally{URL.revokeObjectURL(url);if(seq===this.fileSequence)this.readingSingle=false}
    },
    pasteSinglePhoto(event){
      if(this.photoSourceId)return;
      const file=Array.from(event.clipboardData?.files||[]).find(isPaperPhoto);
      if(file){event.preventDefault();this.clearPdf();this.fileSequence++;this.readingSingle=false;this.wordChoices=[];this.singleMatch=null;this.photo=file;this.singleError=''}
    },
    async chooseQuestionFile(event){
      const file=event.target.files[0];if(!file)return;
      this.clearPdf();const seq=++this.fileSequence;
      this.newAnswer=this.newAnalysis='';this.photo=null;this.singleMatch=null;this.wordChoices=[];this.wordNumber=0;this.singleError='';this.readingSingle=true;
      try{
        if(/\.pdf$/i.test(file.name)){await this.openSinglePdf(file,seq)}
        else if(/\.docx$/i.test(file.name)){
          const rows=await parsePaper(await file.arrayBuffer(),true);
          if(seq!==this.fileSequence)return;
          this.wordChoices=[...rows.values()].filter(r=>r.plain.trim()||r.content.includes('<img'));
          if(!this.wordChoices.length)throw Error('没有可收录的题面');
          this.selectWord(this.wordChoices[0].n);
        }else if(/\.txt$/i.test(file.name)){
          const text=await file.text();if(seq===this.fileSequence)this.newStem=text;
        }else if(isPaperPhoto(file))this.photo=file;
        else throw Error('请选择 DOCX、PDF、TXT 或图片');
      }catch(e){if(seq===this.fileSequence)this.singleError='文件读取失败：'+e.message}
      finally{if(seq===this.fileSequence)this.readingSingle=false}
    },
    selectWord(n){
      this.wordNumber=n;this.singleMatch=null;this.newAnswer=this.newAnalysis='';
      const row=this.singleWord;
      this.newSource=printedQuestionSource(row.plain||row.stem||'');
      this.newType=['单选题','多选题','实验题','填空题','判断题','非选择题'].includes(row.type)?row.type:'单选题';
      if(this.singlePaper)this.singleNo=String(n);
    },
    async collectQuestion(choice=''){
      if(!this.singleComplete){this.singleError='请先补齐答案和解析。';return}
      if(this.readingSingle || (this.pdfDoc&&!this.photo))return;
      try{await this.ensureQuestions()}catch(error){this.singleError=error.message;return}
      const row=this.singleWord;
      const stem=row?(row.stem||row.plain.trim().split('\n')[0]||'图片题'):this.newStem.trim();
      if(!stem || (this.photoSourceId&&!this.appliedPhoto)){this.singleError='请先转写这道题的题面。';return}
      if(this.singlePaper&&this.qs.some(q=>q.src?.some(s=>s[0]===this.singlePaper&&String(s[1])===this.singleNo.trim()))){this.singleError='这份卷已收录该题号，请核对。';return}
      const embedded=printedQuestionSource(stem);
      const question={id:selectionUUID(),manual:true,collectedAt:new Date().toLocaleDateString('sv-SE')+'T'+new Date().toTimeString().slice(0,8),stem,body:stem,k:[],src:this.singlePaper?[[this.singlePaper,this.singleNo.trim()]]:[],origin:this.newSource.trim()||embedded,type:this.newType,level:'',date:new Date().toLocaleDateString('sv-SE'),answer:this.newAnswer.trim(),a:this.newAnalysis.trim()};
      try{
        if(this.photoSourceId)question.photoSourceId=this.photoSourceId;
        if(row)Object.assign(question,{body:row.plain,contentHtml:row.content,answerHtml:hasSolution(row.answer)?row.answer:'',analysisHtml:hasSolution(row.analysis)?row.analysis:''});
        if(this.photo && this.appliedPhoto){
          question.manual=false;question.referencePhoto=this.appliedPhoto.picture;
          question.contentHtml='<p>'+escapeDoc(stem).replaceAll('\n','<br>')+'</p>';
          if(this.appliedPhoto.includeFigure)question.contentHtml+='<img alt="题目选区" src="'+(this.appliedPhoto.figure||this.appliedPhoto.picture)+'">';
        }else if(this.photo){
          question.manual=false;question.contentHtml='<p>'+escapeDoc(stem)+'</p><img alt="题目配图" src="'+await imageData(this.photo)+'">';
        }
        const found=await this.findDuplicate(question),match=found?.q;
        if(match && this.singleMatch?.id!==match.id){this.singleMatch=match;this.singleMatchNote=this.dupNote(found);return}
        if(match && choice==='link'){
          const record={id:question.id,questions:[],links:[{q:question,target:match.id}]};
          await accessRecords('readwrite',store=>store.add(record));this.attachRecord(record);
          this.$refs.collector.close();this.openDetail(match);return
        }
        if(row || this.photo || libraryServer){question.manual=false;await accessRecords('readwrite',store=>store.add({id:question.id,questions:[question]}))}
        this.qs.unshift(question);this.save()
      }catch(e){this.singleError='保存失败：'+e.message;return}
      this.clearFilters();this.order='new';this.pageNo=1;this.$refs.collector.close();this.openDetail(question);void this.classifyQuestions([question])
    },
    primaryType,
    setPage(n){this.pageNo=n;this.$nextTick(()=>this.$refs.filters?.scrollIntoView({block:'start'}))},
    setPageSize(n){this.pageSize=Number(n)||10;this.pageNo=1;try{localStorage.setItem('biofolio-page-size',String(this.pageSize))}catch{}},
    clearBasket(){
      if(!this.bag.length)return;
      if(!confirm('清空试题篮里的全部 '+this.bag.length+' 道题？'))return;
      const old=this.bag;this.bag=[];
      if(this.persist(()=>{this.bag=old}))this.notify('已清空试题篮');
    },
    toggleWrong(q){
      const id=typeof q==='string'?q:q?.id;
      if(!this.canManage(q)){this.notify('仅管理员或收录该题的老师可修改全库易错标记');return false}
      return this.toggle('wrong',id);
    },
    photoPlaceholder(q){return /^[^\n]+\.(?:jpe?g|png|webp|gif|heic)$/i.test((q.stem||'').trim()) && (q.photoPlaceholder || !!q.contentHtml?.includes('<img'))},
    bankReady(q){return !this.photoPlaceholder(q) && !this.missing(q).length},
    hasField(q,f){
      if(f==='source')return Boolean(this.originOf(q));
      const values=f==='answer'?[q.answer,q.answerHtml]:[q.a,q.analysisHtml];
      return [this.fixes[q.id]?.[f],...values].some(v=>v && hasSolution(v));
    },
    missing(q){return ['answer','analysis'].filter(f=>!this.hasField(q,f))},
    fieldName(f){return {answer:'答案',analysis:'解析',source:'来源'}[f]},
    beginFix(f,ai){this.aiDraftToken++;this.aiBusy=false;this.aiDraftNotice='';this.editField=f;this.editValue=f==='source'?this.originOf(this.detail):'';this.helper='';
      if(!ai)return;
      const q=this.detail, answer=this.fixes[q.id]?.answer || q.answer || '', field=this.fieldName(f);
      const asText=html=>{const el=document.createElement('template');el.innerHTML=html||'';el.content.querySelectorAll('sup,sub').forEach(n=>n.replaceWith((n.tagName==='SUP'?'^':'_')+'('+n.textContent+')'));el.content.querySelectorAll('p,div,tr,br').forEach(n=>n.append('\n'));el.content.querySelectorAll('td,th').forEach(n=>n.append('　'));return el.content.textContent.trim()};
      this.helper=['请为下面这道高中生物'+q.type+'起草'+field+'，供教师审核。',f==='analysis'?'结合题干和选项说明判断依据。':'只给出参考答案，按原题顺序作答。','如缺图、条件不足或有歧义，请指出，不要猜测。','题面：',aiQuestionParts(q).text,f==='analysis' && (answer || q.answerHtml)?'已有参考答案：'+(answer||asText(q.answerHtml)):''].filter(Boolean).join('\n')
    },
    async copyAITask(){try{await navigator.clipboard.writeText(this.helper);this.notify('已复制题目和要求')}catch{this.$refs.aiTask.focus();this.$refs.aiTask.select();this.notify('已选中文字，请使用系统复制')}},
    acceptFix(){if(this.editField!=='source'&&!this.editValue.trim())return;const id=this.detail.id,old=this.fixes[id];this.fixes[id]={...old,[this.editField]:this.editField==='source'?this.editValue.trim():sciText(this.editValue.trim())};if(!this.persist(()=>{if(old)this.fixes[id]=old;else delete this.fixes[id]}))return;this.editField='';this.helper=''},
    addPurpose(){
      const name=this.newPurpose.trim();
      if(!name)return;
      if(['全部用途','未分类'].includes(name)){this.purposeError='请换一个用途名称。';return}
      if(!this.purposes.includes(name)){
        this.purposeNames.push(name);
        const oldExams=this.examPurposes;
        if(this.newExam&&!this.examPurposes.includes(name))this.examPurposes=[...this.examPurposes,name];
        try{this.save()}catch(error){this.examPurposes=oldExams;this.purposeNames.pop();this.purposeError=error.message;return}
      }
      this.purposeFilter=name;
      this.$refs.purposeDialog.close();
    },
    saveEdition(scope=null,grading=null){
      const purpose=this.paperPurpose.trim();
      if(['全部用途','未分类'].includes(purpose)){this.notify('请换一个用途名称。');return}
      if(!this.bag.length || !this.paperName.trim() || !purpose)return;
      if(this.chosen.length!==this.bag.length){this.notify('请等待已选题目载入后再保存');return}
      const kept=scope && new Set(scope.results.filter(r=>r.keep).map(r=>r.id));
      const selected=kept?this.chosen.filter(q=>kept.has(q.id)):this.chosen;
      if(!selected.length){this.notify('请至少保留一道题');return}
      if(selected.some(q=>!this.bankReady(q))){this.notify('试题篮含待补题，请补全或移出后再组卷。');return}
      if(this.examMode && selected.some(q=>basketSection(q)===0)){
        this.notify('考试用卷不收判断题，请移出判断题或更改用途');return;
      }
      if(this.examMode && !grading){return this.beginGrading(selected,scope)}
      if(grading && JSON.stringify(selected.map(q=>[q.id,q.revision||0]))!==this.gradeIds){this.notify('选题已变化，请重新打开评分设置。');return}
      const item={id:selectionUUID(),isExam:this.examMode,purpose,name:this.paperName.trim(),date:new Date().toLocaleString('zh-CN'),ids:selected.map(q=>q.id),versions:Object.fromEntries(selected.map(q=>[q.id,q.revision||0])),marked:[],stats:[]};
      if(scope)item.scope=scope;
      if(grading)item.grading=grading;
      item.fixes=Object.fromEntries(item.ids.filter(id=>this.fixes[id]).map(id=>[id,{...this.fixes[id]}]));
      this.overviewItem=item;
      this.overviewSignature=this.basketSignature;
      if(grading)this.confirmOverview();
      else this.$nextTick(()=>this.$refs.paperOverview.showModal());
      return true;
    },
    confirmOverview(){
      const item=this.overviewItem;
      if(this.gradeEdition && item){
        this.editions.unshift(item);
        try{this.save()}catch(e){this.editions.shift();this.gradeError=e.message;this.$refs.paperOverview?.close();return}
        this.$refs.paperOverview?.close();this.overviewItem=null;this.closeGradeTab(true);this.gradeDraft=null;this.gradeEdition=null;
        location.hash='review/'+encodeURIComponent(item.id);this.syncPage();return;
      }
      if(!item || this.overviewSignature!==this.basketSignature){this.notify('题目已变更，请返回重新核对。');if(this.page==='评分细则'){this.gradeError=this.msg;this.$refs.paperOverview?.close()}return}
      const draft=this.bag;
      this.editions.unshift(item);this.bag=[];
      try{this.save()}catch(error){this.editions.shift();this.bag=draft;this.notify(error.message);if(this.page==='评分细则'){this.gradeError=error.message;this.$refs.paperOverview?.close()}return}
      this.$refs.paperOverview?.close();if(this.$refs.scopeDialog?.open)this.$refs.scopeDialog.close();
      this.closeGradeTab(true);this.gradeDraft=null;this.overviewItem=null;this.draftMarks=null;this.examOverride=null;
      this.$refs.basket?.close();this.basketDocked=false;this.paperName='';location.hash='review/'+encodeURIComponent(item.id);this.syncPage();
    },
    startEditEditionMeta(edition){
      this.editionEditTarget=edition;
      this.editionEditName=edition.name||'';
      this.editionEditPurpose=edition.purpose||'';
      this.$refs.editionMetaDialog?.showModal();
    },
    saveEditionMeta(){
      const e=this.editionEditTarget,name=this.editionEditName.trim(),purpose=this.editionEditPurpose.trim();
      if(!e||!name)return;
      if(['全部用途','未分类'].includes(purpose)){this.notify('请换一个用途名称。');return}
      const old={name:e.name,purpose:e.purpose};
      e.name=name;e.purpose=purpose||e.purpose;
      if(!this.persist(()=>Object.assign(e,old)))return;
      this.$refs.editionMetaDialog?.close();
      this.notify('已更新试卷信息');
    },
    async reloadEditionToBasket(edition){
      try{await this.ensureQuestions()}catch(error){this.notify(error.message);return}
      const purged=new Set(this.purgedQuestions),byId=new Map(this.qs.map(q=>[q.id,q]));
      const validIds=edition.ids.filter(id=>!purged.has(id)&&byId.has(id)&&this.bankReady(byId.get(id)));
      if(!validIds.length){this.notify('这份试卷中没有可载入试题篮的有效题目');return}
      if(this.bag.length && JSON.stringify(this.bag)!==JSON.stringify(validIds)){
        if(!confirm('当前试题篮已有 '+this.bag.length+' 道题，要用「'+edition.name+'」的 '+validIds.length+' 道题替换并重新编排吗？\n\n原已保存的试卷不会受影响。'))return;
      }
      const oldBag=this.bag;
      this.bag=validIds;
      this.paperName=edition.name.endsWith('（修订）')?edition.name:edition.name+'（修订）';
      this.paperPurpose=edition.purpose||'';
      this.examOverride=!!(edition.grading||edition.isExam);
      this.draftMarks=edition.grading?JSON.parse(JSON.stringify(edition.grading)):null;
      if(!this.persist(()=>{this.bag=oldBag}))return;
      if(window.innerWidth>=1200)this.basketDocked=true;
      this.go('组卷');
      this.notify('已载入 '+validIds.length+' 道题到试题篮，可增删调序后另存新卷');
    },
    editionFace,
    changeEditionFormat(edition,event){
      const old=edition.formatId;edition.formatId=event.target.value;if(!this.persist(()=>{edition.formatId=old;event.target.value=old||''}))return;this.wordNotice={};
    },
    startRenameFormat(f){
      this.formatRenameId=f.id;
      this.formatRenameText=this.formatAliases[f.id]||f.name;
    },
    confirmRenameFormat(f){
      const name=this.formatRenameText.trim();
      if(!name){this.formatRenameId='';return}
      const had=Object.hasOwn(this.formatAliases,f.id),old=this.formatAliases[f.id];
      this.formatAliases={...this.formatAliases,[f.id]:name};
      if(!this.persist(()=>{const copy={...this.formatAliases};if(had)copy[f.id]=old;else delete copy[f.id];this.formatAliases=copy}))return;
      this.formatRenameId='';
      this.formatMessage='模板已重命名为「'+name+'」。';
    },
    removeFormat(f){
      if(!this.canManageFormat(f)){this.notify('仅管理员或上传该模板的老师可以移除');return}
      const label=this.formatAliases[f.id]||f.name;
      if(!confirm('从模板列表中移除「'+label+'」？\n\n此前已绑定该模板的试卷导出时不受影响。'))return;
      const old=this.deletedFormats;
      this.deletedFormats=[...new Set([...old,f.id])];
      if(!this.persist(()=>{this.deletedFormats=old}))return;
      if(this.formatRenameId===f.id)this.formatRenameId='';
      this.formatMessage='已移除模板「'+label+'」。';
    },
    isGradingFormat(id){return isGradingFormatId(id,this.gradingFormats)},
    saveGradingSettings(){
      if(libraryServer && this.authenticatedPage && !this.admin){this.gradingSettingsNotice='只有管理员可以修改全校扫描批改设置。';return}
      const raw=Number(this.gradingSettingsDigits);
      if(!Number.isInteger(raw)||raw<1||raw>16){this.gradingSettingsNotice='学号位数须为 1 到 16 之间的整数。';return}
      const old={digits:this.studentIdDigits,header:this.schoolHeader};
      const nextHeader=String(this.gradingSettingsHeader||'').trim();
      this.studentIdDigits=raw;this.schoolHeader=nextHeader;this.gradingSettingsDigits=raw;this.gradingSettingsHeader=nextHeader;
      if(!this.persist(()=>{this.studentIdDigits=old.digits;this.schoolHeader=old.header;this.gradingSettingsDigits=old.digits;this.gradingSettingsHeader=old.header}))return;
      this.gradingSettingsNotice='已保存：学号 '+raw+' 位'+(nextHeader?'，页眉「'+nextHeader+'」':'')+'。';
    },
    toggleGradingFormat(f){
      if(!this.canManageFormat(f)){this.notify('仅管理员或上传该模板的老师可修改批改版标记');return}
      const label=this.formatAliases[f.id]||f.name;
      const old=this.gradingFormats;
      const on=old.includes(f.id);
      this.gradingFormats=on?old.filter(x=>x!==f.id):[...old,f.id];
      if(!this.persist(()=>{this.gradingFormats=old}))return;
      this.formatMessage=on?'已取消「'+label+'」的批改版标记。':'已将「'+label+'」设为批改版模板（导出学生版时自动加学号方框与每页二维码）。';
    },
    async uploadFormat(event){
      const file=event.target.files[0];event.target.value='';
      if(!file)return;
      this.formatBusy=true;this.formatMessage='';
      try{
        if(!/\.docx$/i.test(file.name))throw Error('请选择 .docx 格式的 Word 模板。');
        const bytes=await file.arrayBuffer();
        const id='format-'+await fileDigest(bytes);
        const existing=this.formats.find(f=>f.id===id);
        if(existing){
          if(this.deletedFormats.includes(id)){
            const old=this.deletedFormats;this.deletedFormats=old.filter(x=>x!==id);
            this.persist(()=>{this.deletedFormats=old});
            this.formatMessage='已恢复模板「'+(this.formatAliases[id]||existing.name)+'」。';return;
          }
          this.formatMessage='模板库已有「'+(this.formatAliases[id]||existing.name)+'」，可以直接选用。';return;
        }
        const template=await prepareWordTemplate(bytes);
        const record={id,wordTemplate:true,name:file.name.replace(/\.docx$/i,''),...template};
        await accessRecords('readwrite',store=>store.put(record));
        this.attachRecord(record);
        if(template.hasStudentBox && !this.gradingFormats.includes(id)){
          const old=this.gradingFormats;this.gradingFormats=[...old,id];
          this.persist(()=>{this.gradingFormats=old});
          this.formatMessage='已添加「'+record.name+'」（检测到 {{学号方框}}，已自动设为批改版模板）。';
        }else{
          this.formatMessage='已添加「'+record.name+'」。';
        }
      }catch(error){this.formatMessage='模板未导入：'+error.message}finally{this.formatBusy=false}
    },
    async downloadSampleTemplate(){
      try{
        const blob=await makeSampleTemplate();
        const url=URL.createObjectURL(blob),link=document.createElement('a');
        link.href=url;link.download='生物试卷示例模板.docx';link.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
      }catch(error){this.formatMessage='示例模板生成失败：'+error.message}
    },
    async downloadGradingSampleTemplate(){
      try{
        const blob=await makeGradingSampleTemplate(this.schoolHeader);
        const url=URL.createObjectURL(blob),link=document.createElement('a');
        link.href=url;link.download='生物试卷批改版示例模板.docx';link.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
      }catch(error){this.formatMessage='批改版示例模板生成失败：'+error.message}
    },
    collectEditionExportItems(edition,requireAnalysis=true){
      return edition.ids.map((id,i)=>{
        const q=this.editionQuestion(id,edition);
        if(q && this.conflictingLinks(q).length)throw Error('第 '+(i+1)+' 题存在答案冲突，请先核对。');
        if(!q)throw Error('第 '+(i+1)+' 题尚未载入，请稍后再试。');
        const fix=edition.fixes?.[id]||this.fixes[id]||{};
        const answer=q.answerHtml||plainBlocks(fix.answer||q.answer);
        const analysis=q.analysisHtml||plainBlocks(fix.analysis||q.a);
        if(!(q.answerHtml||fix.answer||q.answer)||(requireAnalysis&&!(q.analysisHtml||fix.analysis||q.a)))throw Error('第 '+(i+1)+' 题缺少答案或解析，请补齐后再导出。');
        return {q,answer,analysis,source:fix.source??q.origin};
      });
    },
    async downloadEdition(edition,mode){
      if(typeof mode==='boolean')mode=mode?'teacher':'student';
      if(this.wordBusy)return;
      this.wordBusy=true;this.wordNotice={id:edition.id,text:'正在生成 Word…'};
      try{
        await this.ensureQuestions();
        const items=this.collectEditionExportItems(edition,mode!=='answers');
        const isBuiltinGrading=edition.formatId===BUILTIN_GRADING_FORMAT_ID;
        const template=(edition.formatId && !isBuiltinGrading)?await accessRecords('readonly',store=>store.get(edition.formatId)):null;
        if(edition.formatId && !isBuiltinGrading && !template)throw Error('找不到所选模板，请重新上传。');
        const gradingMode=isGradingFormatId(edition.formatId,this.gradingFormats);
        let gradingOpts=null;
        if(gradingMode){
          if(mode==='student'){
            const paperId=await computePaperId(edition);
            const templateHeader=template?await extractTemplateHeaderText(template.file):'';
            const effectiveHeader=templateHeader || this.schoolHeader || '';
            const layout=await paginateStudentPaper(edition,items,{studentIdDigits:this.studentIdDigits,schoolHeader:effectiveHeader,safetyRatio:0.88});
            gradingOpts={enabled:true,paperId,studentIdDigits:this.studentIdDigits,schoolHeader:effectiveHeader,pages:layout.pages};
          }else{
            gradingOpts={enabled:true,schoolHeader:this.schoolHeader||''};
          }
        }
        const blob=await makeEditionWord(edition,items,mode,template,gradingOpts);
        const url=URL.createObjectURL(blob),link=document.createElement('a');
        link.href=url;link.download=edition.name.replace(/[\\/:*?"<>|]/g,'_')+'（'+({teacher:'解析版',answers:'答案及评分标准'}[mode]||'学生版')+'）.docx';
        link.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
        this.wordNotice={};
        if(gradingMode && mode==='student' && gradingOpts?.pages){
          this.notify('已导出批改版学生 Word（'+gradingOpts.pages.length+' 页，卷号 '+gradingOpts.paperId+'）');
        }
      }catch(error){this.wordNotice={id:edition.id,text:error.message}}finally{this.wordBusy=false}
    },
    async downloadGradingPack(edition){
      if(this.wordBusy)return;
      this.wordBusy=true;this.wordNotice={id:edition.id,text:'正在生成批改包（JSON）…'};
      try{
        await this.ensureQuestions();
        const items=this.collectEditionExportItems(edition,false);
        const paperId=await computePaperId(edition);
        let effectiveHeader=this.schoolHeader||'';
        if(edition.formatId && edition.formatId!==BUILTIN_GRADING_FORMAT_ID && isGradingFormatId(edition.formatId,this.gradingFormats)){
          const tpl=await accessRecords('readonly',store=>store.get(edition.formatId));
          if(tpl?.file){const extracted=await extractTemplateHeaderText(tpl.file);if(extracted)effectiveHeader=extracted}
        }
        const layout=await paginateStudentPaper(edition,items,{studentIdDigits:this.studentIdDigits,schoolHeader:effectiveHeader,safetyRatio:0.98});
        const pack=buildGradingPack(edition,items,{
          paperId,
          studentIdDigits:this.studentIdDigits,
          pageCount:layout.pageCount,
          questionPages:layout.questionPages
        });
        const safeName=(edition.name||'生物试卷').replace(/[\\/:*?"<>|]/g,'_');
        const blob=new Blob([JSON.stringify(pack,null,2)],{type:'application/json;charset=utf-8'});
        const url=URL.createObjectURL(blob),link=document.createElement('a');
        link.href=url;link.download='批改包-'+safeName+'-'+paperId+'.json';
        link.click();setTimeout(()=>URL.revokeObjectURL(url),30000);
        this.wordNotice={};
        this.notify(edition.grading
          ? '已导出批改包（'+paperId+'，共 '+(layout.pageCount||'?')+' 页）'
          : '已按默认分值导出批改包（'+paperId+'）；如需逐空细则请先在阅读页设置评分细则');
      }catch(error){this.wordNotice={id:edition.id,text:error.message}}finally{this.wordBusy=false}
    },
    markReview(id){
      const review=this.review,marked=review.marked,wrong=this.wrong,was=marked.includes(id);
      review.marked=this.flip(marked,id);
      if(!was&&!wrong.includes(id)&&this.canManage(id))this.wrong=[...wrong,id];
      this.persist(()=>{review.marked=marked;this.wrong=wrong})
    },
    formatScoreNum(v){return Number.isFinite(v)?Number(v.toFixed(1)):''},
    resetScores(){this.scoreSources=[];this.scoreInput='';this.scoreMessage='';this.scoreNotice='';this.scorePreview=[];this.scoreGaps=[];this.scoreAutoMaxCols=[];this.scoreMaxDraft={};this.scoreMaxTouched={};this.scoreMarks=[]},
    onScoreInputChange(){this.scorePreview=[];this.scoreGaps=[];this.scoreAutoMaxCols=[];this.scoreMaxDraft={};this.scoreMaxTouched={};this.scoreMarks=[];this.scoreMessage='';this.scoreNotice=''},
    removeScoreSource(idx){this.scoreSources.splice(idx,1);if(!this.scoreSources.length)this.resetScores();else return this.previewScores()},
    updateScoreMax(n,raw){this.scoreMaxTouched={...this.scoreMaxTouched,[n]:true};this.scoreMaxDraft={...this.scoreMaxDraft,[n]:raw};return this.previewScores(true)},
    applyScoreThreshold(){const t=Math.max(0,Math.min(100,Number(this.scoreThreshold)||0));this.scoreMarks=this.scorePreview.filter(r=>r.rate!==null&&r.missed>0&&r.rate*100+1e-9>=t).map(r=>r.n)},
    async readScores(event){
      const files=Array.from(event.target.files||[]);
      event.target.value='';
      if(!files.length)return;
      if(!this.scoreSources.length)this.scoreInput='';
      this.scoreMessage='';
      const next=[...this.scoreSources];
      try{
        for(const file of files){
          const key=file.name+':'+file.size+':'+file.lastModified;
          const idx=next.findIndex(s=>s.key===key);
          let entry;
          if(/\.csv$/i.test(file.name)){
            entry={key,name:file.name,kind:'csv',text:decodeScoreCsvBytes(await file.arrayBuffer())};
          }else{
            await loadTool('xlsx');
            const book=Vue.markRaw(XLSX.read(await file.arrayBuffer(),{type:'array'}));
            entry={key,name:file.name,kind:'xlsx',book,sheets:book.SheetNames,sheet:book.SheetNames[0]||''};
          }
          if(idx>=0)next.splice(idx,1,entry);
          else next.push(entry);
        }
        this.scoreSources=next;
        await this.previewScores();
      }catch(e){
        this.scoreMessage=e?.message&&/[\u4e00-\u9fa5]/.test(e.message)?e.message:'文件读取失败，请检查格式，或粘贴表格。';
      }
    },
    async previewScores(keepMarks=false){
      this.scorePreview=[];this.scoreGaps=[];this.scoreMessage='';this.scoreNotice='';
      if(!this.review)return;
      const nLen=this.review.ids.length;
      try{
        await this.ensureQuestions();
        await loadTool('xlsx');
        const blocks=[];
        if(this.scoreSources.length){
          for(const src of this.scoreSources){
            if(src.kind==='csv'){
              const raw=src.text.trim();
              if(!raw)continue;
              const book=XLSX.read(raw,{type:'string',raw:true});
              const rows=XLSX.utils.sheet_to_json(book.Sheets[book.SheetNames[0]],{header:1,defval:''});
              blocks.push(parseScoreBlock(rows,nLen));
            }else{
              const names=src.sheet==='*'?src.sheets:[src.sheet||src.sheets[0]];
              let matched=0,lastHeaderErr=null;
              for(const name of names){
                const sheetObj=src.book.Sheets[name];
                if(!sheetObj)continue;
                const rows=XLSX.utils.sheet_to_json(sheetObj,{header:1,defval:''});
                if(src.sheet==='*'){
                  try{blocks.push(parseScoreBlock(rows,nLen));matched++}
                  catch(err){if(/^(?:未识别表头|表格需要表头)/.test(err.message)){lastHeaderErr=err;continue}throw err}
                }else{
                  blocks.push(parseScoreBlock(rows,nLen));matched++;
                }
              }
              if(!matched)throw lastHeaderErr||Error('未识别表头。请用下方所列的逐题表、错题号表或得分表格式。');
            }
          }
        }else{
          const raw=this.scoreInput.trim();
          if(!raw){this.scoreAutoMaxCols=[];return}
          const book=XLSX.read(raw,{type:'string',raw:true});
          const rows=XLSX.utils.sheet_to_json(book.Sheets[book.SheetNames[0]],{header:1,defval:''});
          blocks.push(parseScoreBlock(rows,nLen));
        }
        if(!blocks.length)throw Error('表格需要表头和至少一行数据。');
        const overrides={};
        for(const [k,touched] of Object.entries(this.scoreMaxTouched)){
          if(touched)overrides[k]=this.scoreMaxDraft[k];
        }
        const spec=buildScoreSpec(this.review,(id,ed)=>this.editionQuestion(id,ed),this.fixes,overrides);
        const res=evaluateScoreDatasets(blocks,nLen,spec);
        if(!res.hasFileMax&&res.autoMaxCols.length){
          const draft={};
          for(const n of res.autoMaxCols)draft[n]=this.scoreMaxTouched[n]?this.scoreMaxDraft[n]:String(res.inferredMax[n]??'');
          this.scoreAutoMaxCols=res.autoMaxCols;
          this.scoreMaxDraft=draft;
        }else{
          this.scoreAutoMaxCols=[];this.scoreMaxDraft={};this.scoreMaxTouched={};
        }
        const notes=[];
        if(res.examineeCount){
          const srcCount=this.scoreSources.length;
          notes.push((srcCount>1?'已合并 '+srcCount+' 份成绩表，共 ':'共 ')+res.examineeCount+' 人。');
        }
        if(res.adaptedFromGrading){
          notes.push('检测到实考单题最高分与本卷原设分值不同，已按实考数据预填非选择题满分，可在下方核对微调。');
        }
        this.scoreNotice=notes.join(' ');
        const seenNumbers=new Set(res.items.map(r=>r.n));
        // Questions absent from a per-question table had no recorded losses; show them so the teacher can check.
        this.scoreGaps=Array.from({length:nLen},(_,i)=>i+1).filter(n=>!seenNumbers.has(n));
        this.scorePreview=res.items.map(r=>({
          ...r,
          id:this.review.ids[r.n-1],
          rate:r.lossRate??(r.total?r.missed/r.total:null)
        })).sort((a,b)=>(b.rate??-1)-(a.rate??-1)||b.missed-a.missed||a.n-b.n);
        if(!keepMarks){
          if(this.scorePreview.some(r=>r.rate!==null))this.applyScoreThreshold();
          else this.scoreMarks=[];
        }else{
          const validNums=new Set(this.scorePreview.map(r=>r.n));
          this.scoreMarks=this.scoreMarks.filter(n=>validNums.has(n));
        }
      }catch(e){
        this.scoreMessage=e.message;
        if(e.autoMaxCols?.length){
          const draft={};
          for(const n of e.autoMaxCols)draft[n]=this.scoreMaxTouched[n]?this.scoreMaxDraft[n]:String(e.inferredMax?.[n]??'');
          this.scoreAutoMaxCols=e.autoMaxCols;
          this.scoreMaxDraft=draft;
        }else{
          this.scoreAutoMaxCols=[];
        }
        if(!keepMarks)this.scoreMarks=[];
      }
    },
    applyScores(){
      const review=this.review;
      if(!review)return;
      const oldStats=review.stats,oldMarked=review.marked,oldWrong=this.wrong;
      review.stats=this.scorePreview.map(r=>({
        n:r.n,missed:r.missed,total:r.total,
        ...(r.lossRate!=null?{lossRate:Number(r.lossRate.toFixed(4))}:{}),
        ...(r.max?{max:r.max}:{})
      }));
      const markSet=new Set(review.marked||[]),wrongSet=new Set(this.wrong||[]);
      let newlyWrong=0,skippedShared=0;
      for(const n of this.scoreMarks){
        const id=review.ids[n-1];
        if(!id)continue;
        markSet.add(id);
        if(!wrongSet.has(id)){
          if(this.canManage(id)){wrongSet.add(id);newlyWrong++}
          else skippedShared++;
        }
      }
      if(this.scoreMarks.length){review.marked=[...markSet];this.wrong=[...wrongSet]}
      if(!this.persist(()=>{review.stats=oldStats;review.marked=oldMarked;this.wrong=oldWrong}))return;
      const markedCount=this.scoreMarks.length;
      this.resetScores();this.scoreTab=false;
      if(markedCount){
        let msg='已保存成绩汇总，并标出 '+markedCount+' 道本卷易错题';
        if(newlyWrong)msg+='（'+newlyWrong+' 道新加入全库易错）';
        if(skippedShared)msg+='；另有 '+skippedShared+' 道非本人收录题仅标本卷易错';
        this.notify(msg);
      }else{
        this.notify('已按本卷题号保存匿名汇总');
      }
    }
  }
};

const QuestionNav = {
  props: {ids: {type:Array, required:true}},
  data(){return {current:1, frame:0}},
  template: `<nav class="question-nav" aria-label="题号导航">
    <small>题号</small><div class="numbers" ref="numbers">
      <button v-for="(id,i) in ids" :key="id" :aria-label="'跳到第 '+(i+1)+' 题'"
        :aria-current="current===i+1?'location':null" @click="jump(i+1)">{{i+1}}</button>
    </div></nav>`,
  mounted(){
    window.addEventListener('scroll',this.queueUpdate,{passive:true});
    window.addEventListener('resize',this.queueUpdate);
    this.queueUpdate();
  },
  beforeUnmount(){
    window.removeEventListener('scroll',this.queueUpdate);
    window.removeEventListener('resize',this.queueUpdate);
    cancelAnimationFrame(this.frame);
  },
  methods:{
    jump(n){
      document.getElementById('review-question-'+n)?.scrollIntoView({block:'start'});
      this.queueUpdate();
    },
    queueUpdate(){
      if(this.frame)return;
      this.frame=requestAnimationFrame(()=>{
        this.frame=0;
        const boundary=Math.max(61,this.$el.getBoundingClientRect().bottom+2);
        let first=1,last=this.ids.length;
        while(first<last){
          const mid=Math.ceil((first+last)/2);
          const question=document.getElementById('review-question-'+mid);
          if(question && question.getBoundingClientRect().top<=boundary)first=mid;
          else last=mid-1;
        }
        if(this.current===first)return;
        this.current=first;
        const strip=this.$refs.numbers, button=strip.children[first-1];
        const bounds=strip.getBoundingClientRect(),target=button.getBoundingClientRect();
        if(target.right>bounds.right)strip.scrollLeft+=target.right-bounds.right;
        else if(target.left<bounds.left)strip.scrollLeft+=target.left-bounds.left;
      });
    }
  }
};
