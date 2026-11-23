// Zero-dependency offline ISO/IEC 18004 QR Code encoder (Versions 1-6, EC L/M/Q/H).
// Produces exact boolean matrices, crisp SVG markup for A4 PDF printing, and
// deterministic 8-bit grayscale PNG bytes for Word (.docx) header embedding.
(function(global){
  const ALNUM = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:';
  // ISO 18004 Table 12: format error-correction indicator bits.
  const EC_BITS = { L: 1, M: 0, Q: 3, H: 2 };
  // Versions 1..6 RS block layouts: [ecPerBlock, g1Blocks, g1Data, g2Blocks, g2Data].
  const RS_TABLE = {
    L: [
      null,
      [7, 1, 19, 0, 0],
      [10, 1, 34, 0, 0],
      [15, 1, 55, 0, 0],
      [20, 1, 80, 0, 0],
      [26, 1, 108, 0, 0],
      [18, 2, 68, 0, 0]
    ],
    M: [
      null,
      [10, 1, 16, 0, 0],
      [16, 1, 28, 0, 0],
      [26, 1, 44, 0, 0],
      [18, 2, 32, 0, 0],
      [24, 2, 43, 0, 0],
      [16, 4, 27, 0, 0]
    ],
    Q: [
      null,
      [13, 1, 13, 0, 0],
      [22, 1, 22, 0, 0],
      [18, 2, 17, 0, 0],
      [26, 2, 24, 0, 0],
      [18, 2, 15, 2, 16],
      [24, 4, 19, 0, 0]
    ],
    H: [
      null,
      [17, 1, 9, 0, 0],
      [28, 1, 16, 0, 0],
      [22, 2, 13, 0, 0],
      [16, 4, 9, 0, 0],
      [22, 2, 11, 2, 12],
      [28, 4, 15, 0, 0]
    ]
  };
  const ALIGN = [null, [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34]];

  // GF(256) with primitive polynomial 0x11D.
  const GF_EXP = new Uint8Array(512);
  const GF_LOG = new Uint8Array(256);
  for (let i = 0, x = 1; i < 255; i++) {
    GF_EXP[i] = x;
    GF_EXP[i + 255] = x;
    GF_LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  function gfMul(a, b) {
    return a && b ? GF_EXP[GF_LOG[a] + GF_LOG[b]] : 0;
  }
  function rsGenerator(degree) {
    let poly = [1];
    for (let i = 0; i < degree; i++) {
      const next = new Array(poly.length + 1).fill(0);
      const factor = GF_EXP[i];
      for (let j = 0; j < poly.length; j++) {
        next[j] ^= poly[j];
        next[j + 1] ^= gfMul(poly[j], factor);
      }
      poly = next;
    }
    return poly;
  }
  function rsRemainder(data, ecCount) {
    const gen = rsGenerator(ecCount);
    const rem = new Uint8Array(ecCount);
    for (let i = 0; i < data.length; i++) {
      const factor = data[i] ^ rem[0];
      rem.copyWithin(0, 1);
      rem[ecCount - 1] = 0;
      if (factor) {
        for (let j = 0; j < ecCount; j++) rem[j] ^= gfMul(gen[j + 1], factor);
      }
    }
    return rem;
  }

  function isAlphanumeric(str) {
    for (let i = 0; i < str.length; i++) {
      if (ALNUM.indexOf(str[i]) < 0) return false;
    }
    return str.length > 0;
  }

  function encodePayloadBits(text) {
    const bits = [];
    const push = (val, len) => {
      for (let i = len - 1; i >= 0; i--) bits.push((val >>> i) & 1);
    };
    if (isAlphanumeric(text)) {
      push(0b0010, 4);
      push(text.length, 9);
      for (let i = 0; i < text.length; i += 2) {
        const a = ALNUM.indexOf(text[i]);
        if (i + 1 < text.length) {
          push(a * 45 + ALNUM.indexOf(text[i + 1]), 11);
        } else {
          push(a, 6);
        }
      }
    } else {
      const bytes = new TextEncoder().encode(text);
      push(0b0100, 4);
      push(bytes.length, 8);
      for (const b of bytes) push(b, 8);
    }
    return bits;
  }

  function pickVersion(bitLen, ecLevel) {
    const table = RS_TABLE[ecLevel];
    if (!table) throw Error('不支持的二维码纠错等级：' + ecLevel);
    for (let v = 1; v <= 6; v++) {
      const [, g1b, g1d, g2b, g2d] = table[v];
      const capBits = (g1b * g1d + g2b * g2d) * 8;
      if (bitLen <= capBits) return v;
    }
    throw Error('二维码内容超出 Version 6 容量限制');
  }

  function buildCodewords(bits, version, ecLevel) {
    const [ecLen, g1b, g1d, g2b, g2d] = RS_TABLE[ecLevel][version];
    const totalData = g1b * g1d + g2b * g2d;
    const capBits = totalData * 8;
    const padded = bits.slice();
    const term = Math.min(4, capBits - padded.length);
    for (let i = 0; i < term; i++) padded.push(0);
    while (padded.length % 8) padded.push(0);
    const data = [];
    for (let i = 0; i < padded.length; i += 8) {
      let b = 0;
      for (let j = 0; j < 8; j++) b = (b << 1) | padded[i + j];
      data.push(b);
    }
    for (let i = 0; data.length < totalData; i++) data.push(i % 2 === 0 ? 0xec : 0x11);

    const blocks = [];
    const ecs = [];
    let offset = 0;
    for (let i = 0; i < g1b; i++) {
      const blk = data.slice(offset, offset + g1d);
      offset += g1d;
      blocks.push(blk);
      ecs.push(rsRemainder(blk, ecLen));
    }
    for (let i = 0; i < g2b; i++) {
      const blk = data.slice(offset, offset + g2d);
      offset += g2d;
      blocks.push(blk);
      ecs.push(rsRemainder(blk, ecLen));
    }

    const out = [];
    const maxData = Math.max(g1d, g2d);
    for (let i = 0; i < maxData; i++) {
      for (const blk of blocks) if (i < blk.length) out.push(blk[i]);
    }
    for (let i = 0; i < ecLen; i++) {
      for (const ec of ecs) out.push(ec[i]);
    }
    return out;
  }

  function formatInfoBits(ecLevel, mask) {
    const data = (EC_BITS[ecLevel] << 3) | mask;
    let rem = data << 10;
    for (let i = 14; i >= 10; i--) {
      if ((rem >>> i) & 1) rem ^= 0x537 << (i - 10);
    }
    return ((data << 10) | rem) ^ 0x5412;
  }

  function maskFn(mask, r, c) {
    switch (mask) {
      case 0: return (r + c) % 2 === 0;
      case 1: return r % 2 === 0;
      case 2: return c % 3 === 0;
      case 3: return (r + c) % 3 === 0;
      case 4: return (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0;
      case 5: return ((r * c) % 2) + ((r * c) % 3) === 0;
      case 6: return (((r * c) % 2) + ((r * c) % 3)) % 2 === 0;
      case 7: return (((r + c) % 2) + ((r * c) % 3)) % 2 === 0;
      default: return false;
    }
  }

  function buildBaseGrid(version) {
    const size = 17 + 4 * version;
    const grid = Array.from({ length: size }, () => new Uint8Array(size));
    const isFunc = Array.from({ length: size }, () => new Uint8Array(size));
    const set = (r, c, dark) => {
      if (r < 0 || c < 0 || r >= size || c >= size) return;
      grid[r][c] = dark ? 1 : 0;
      isFunc[r][c] = 1;
    };
    const drawFinder = (tr, tc) => {
      for (let dr = -1; dr <= 7; dr++) {
        for (let dc = -1; dc <= 7; dc++) {
          const r = tr + dr, c = tc + dc;
          if (r < 0 || c < 0 || r >= size || c >= size) continue;
          const dark = dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6 &&
            (dr === 0 || dr === 6 || dc === 0 || dc === 6 || (dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4));
          set(r, c, dark);
        }
      }
    };
    drawFinder(0, 0);
    drawFinder(0, size - 7);
    drawFinder(size - 7, 0);

    for (let i = 8; i < size - 8; i++) {
      set(6, i, i % 2 === 0);
      set(i, 6, i % 2 === 0);
    }

    const centers = ALIGN[version];
    for (const cr of centers) {
      for (const cc of centers) {
        if (isFunc[cr][cc]) continue;
        for (let dr = -2; dr <= 2; dr++) {
          for (let dc = -2; dc <= 2; dc++) {
            const dark = Math.max(Math.abs(dr), Math.abs(dc)) !== 1;
            set(cr + dr, cc + dc, dark);
          }
        }
      }
    }

    // Reserve format info cells & place mandatory dark module.
    for (let i = 0; i <= 8; i++) {
      if (i !== 6) {
        set(8, i, false);
        set(i, 8, false);
      }
    }
    for (let i = size - 8; i < size; i++) {
      set(8, i, false);
      set(i, 8, false);
    }
    set(4 * version + 9, 8, true);
    return { size, grid, isFunc };
  }

  function applyFormat(grid, size, ecLevel, mask) {
    const bits = formatInfoBits(ecLevel, mask);
    const bit = idx => (bits >>> idx) & 1;
    for (let i = 0; i <= 5; i++) grid[8][i] = bit(14 - i);
    grid[8][7] = bit(8);
    grid[8][8] = bit(7);
    grid[7][8] = bit(6);
    for (let i = 0; i <= 5; i++) grid[5 - i][8] = bit(5 - i);
    for (let i = 0; i <= 6; i++) grid[size - 1 - i][8] = bit(14 - i);
    for (let i = 0; i <= 7; i++) grid[8][size - 8 + i] = bit(7 - i);
  }

  function placeData(base, codewords) {
    const { size, isFunc } = base;
    const grid = base.grid.map(row => row.slice());
    let bitIdx = 0;
    const totalBits = codewords.length * 8;
    let upward = true;
    for (let col = size - 1; col >= 1; col -= 2) {
      if (col === 6) col = 5;
      for (let step = 0; step < size; step++) {
        const row = upward ? size - 1 - step : step;
        for (let dx = 0; dx < 2; dx++) {
          const c = col - dx;
          if (isFunc[row][c]) continue;
          let dark = 0;
          if (bitIdx < totalBits) {
            dark = (codewords[bitIdx >>> 3] >>> (7 - (bitIdx & 7))) & 1;
            bitIdx++;
          }
          grid[row][c] = dark;
        }
      }
      upward = !upward;
    }
    return grid;
  }

  function penaltyScore(grid, size) {
    let score = 0;
    // Rule 1: adjacent modules in row/col of same color (5+).
    for (let r = 0; r < size; r++) {
      let run = 1;
      for (let c = 1; c < size; c++) {
        if (grid[r][c] === grid[r][c - 1]) {
          run++;
          if (run === 5) score += 3;
          else if (run > 5) score += 1;
        } else run = 1;
      }
    }
    for (let c = 0; c < size; c++) {
      let run = 1;
      for (let r = 1; r < size; r++) {
        if (grid[r][c] === grid[r - 1][c]) {
          run++;
          if (run === 5) score += 3;
          else if (run > 5) score += 1;
        } else run = 1;
      }
    }
    // Rule 2: 2x2 blocks of same color.
    for (let r = 0; r < size - 1; r++) {
      for (let c = 0; c < size - 1; c++) {
        const v = grid[r][c];
        if (v === grid[r][c + 1] && v === grid[r + 1][c] && v === grid[r + 1][c + 1]) score += 3;
      }
    }
    // Rule 3: finder-like patterns in rows/cols.
    const matchFinder = (line) => {
      for (let i = 0; i <= size - 11; i++) {
        const a = line[i]===1 && line[i+1]===0 && line[i+2]===1 && line[i+3]===1 && line[i+4]===1 && line[i+5]===0 && line[i+6]===1 && line[i+7]===0 && line[i+8]===0 && line[i+9]===0 && line[i+10]===0;
        const b = line[i]===0 && line[i+1]===0 && line[i+2]===0 && line[i+3]===0 && line[i+4]===1 && line[i+5]===0 && line[i+6]===1 && line[i+7]===1 && line[i+8]===1 && line[i+9]===0 && line[i+10]===1;
        if (a || b) score += 40;
      }
    };
    for (let r = 0; r < size; r++) matchFinder(grid[r]);
    const colBuf = new Uint8Array(size);
    for (let c = 0; c < size; c++) {
      for (let r = 0; r < size; r++) colBuf[r] = grid[r][c];
      matchFinder(colBuf);
    }
    // Rule 4: dark ratio balance.
    let dark = 0;
    for (let r = 0; r < size; r++) for (let c = 0; c < size; c++) dark += grid[r][c];
    const ratio = Math.abs((dark * 100) / (size * size) - 50);
    score += Math.floor(ratio / 5) * 10;
    return score;
  }

  function qrMatrix(text, ecLevel = 'Q') {
    const content = String(text ?? '');
    if (!content) throw Error('二维码内容不能为空');
    const bits = encodePayloadBits(content);
    const version = pickVersion(bits.length, ecLevel);
    const codewords = buildCodewords(bits, version, ecLevel);
    const base = buildBaseGrid(version);
    const unmasked = placeData(base, codewords);

    let bestGrid = null;
    let bestScore = Infinity;
    for (let mask = 0; mask < 8; mask++) {
      const candidate = unmasked.map(row => row.slice());
      for (let r = 0; r < base.size; r++) {
        for (let c = 0; c < base.size; c++) {
          if (!base.isFunc[r][c] && maskFn(mask, r, c)) candidate[r][c] ^= 1;
        }
      }
      applyFormat(candidate, base.size, ecLevel, mask);
      const s = penaltyScore(candidate, base.size);
      if (s < bestScore) {
        bestScore = s;
        bestGrid = candidate;
      }
    }
    return { version, size: base.size, modules: bestGrid };
  }

  function qrSvgString(text, opts = {}) {
    const quiet = opts.quiet ?? 4;
    const ecLevel = opts.ecLevel || 'Q';
    const { size, modules } = qrMatrix(text, ecLevel);
    const total = size + quiet * 2;
    const parts = [];
    for (let r = 0; r < size; r++) {
      let c = 0;
      while (c < size) {
        if (!modules[r][c]) { c++; continue; }
        let run = 1;
        while (c + run < size && modules[r][c + run]) run++;
        parts.push(`M${c + quiet},${r + quiet}h${run}v1h-${run}z`);
        c += run;
      }
    }
    const cls = opts.className ? ` class="${opts.className}"` : '';
    const attrs = opts.sizeMm ? ` width="${opts.sizeMm}mm" height="${opts.sizeMm}mm"` : '';
    return `<svg xmlns="http://www.w3.org/2000/svg"${cls}${attrs} viewBox="0 0 ${total} ${total}" shape-rendering="crispEdges"><rect width="${total}" height="${total}" fill="#fff"/><path d="${parts.join('')}" fill="#000"/></svg>`;
  }

  // Minimal CRC32 + Adler32 + Stored-DEFLATE 8-bit grayscale PNG encoder.
  const CRC_TABLE = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = (c & 1) ? (0xedb88320 ^ (c >>> 1)) : (c >>> 1);
    CRC_TABLE[n] = c >>> 0;
  }
  function crc32(bytes) {
    let c = 0xffffffff;
    for (let i = 0; i < bytes.length; i++) c = CRC_TABLE[(c ^ bytes[i]) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
  }
  function adler32(bytes) {
    let a = 1, b = 0;
    for (let i = 0; i < bytes.length; i++) {
      a = (a + bytes[i]) % 65521;
      b = (b + a) % 65521;
    }
    return ((b << 16) | a) >>> 0;
  }
  function pngChunk(type, data) {
    const out = new Uint8Array(12 + data.length);
    const dv = new DataView(out.buffer);
    dv.setUint32(0, data.length, false);
    for (let i = 0; i < 4; i++) out[4 + i] = type.charCodeAt(i);
    out.set(data, 8);
    dv.setUint32(8 + data.length, crc32(out.subarray(4, 8 + data.length)), false);
    return out;
  }
  function zlibStored(raw) {
    const MAX = 65535;
    const blockCount = Math.max(1, Math.ceil(raw.length / MAX));
    const out = new Uint8Array(2 + raw.length + blockCount * 5 + 4);
    out[0] = 0x78;
    out[1] = 0x01;
    let pos = 2, off = 0;
    while (off < raw.length || pos === 2) {
      const len = Math.min(MAX, raw.length - off);
      const last = off + len >= raw.length ? 1 : 0;
      out[pos++] = last;
      out[pos++] = len & 0xff;
      out[pos++] = (len >>> 8) & 0xff;
      const nlen = (~len) & 0xffff;
      out[pos++] = nlen & 0xff;
      out[pos++] = (nlen >>> 8) & 0xff;
      out.set(raw.subarray(off, off + len), pos);
      pos += len;
      off += len;
    }
    new DataView(out.buffer).setUint32(pos, adler32(raw), false);
    return out;
  }

  function qrPngBytes(text, opts = {}) {
    const quiet = opts.quiet ?? 4;
    const scale = opts.scale ?? 8;
    const ecLevel = opts.ecLevel || 'Q';
    const { size, modules } = qrMatrix(text, ecLevel);
    const totalMods = size + quiet * 2;
    const px = totalMods * scale;
    const stride = 1 + px;
    const raw = new Uint8Array(stride * px);
    raw.fill(255);
    for (let y = 0; y < px; y++) {
      raw[y * stride] = 0; // PNG filter type 0 (None)
    }
    for (let r = 0; r < size; r++) {
      for (let c = 0; c < size; c++) {
        if (!modules[r][c]) continue;
        const y0 = (r + quiet) * scale;
        const x0 = (c + quiet) * scale;
        for (let dy = 0; dy < scale; dy++) {
          const rowStart = (y0 + dy) * stride + 1 + x0;
          raw.fill(0, rowStart, rowStart + scale);
        }
      }
    }
    const ihdr = new Uint8Array(13);
    const dv = new DataView(ihdr.buffer);
    dv.setUint32(0, px, false);
    dv.setUint32(4, px, false);
    ihdr[8] = 8; // bit depth 8
    ihdr[9] = 0; // grayscale
    ihdr[10] = 0;
    ihdr[11] = 0;
    ihdr[12] = 0;
    const sig = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]);
    const c1 = pngChunk('IHDR', ihdr);
    const c2 = pngChunk('IDAT', zlibStored(raw));
    const c3 = pngChunk('IEND', new Uint8Array(0));
    const png = new Uint8Array(sig.length + c1.length + c2.length + c3.length);
    let p = 0;
    for (const part of [sig, c1, c2, c3]) { png.set(part, p); p += part.length; }
    return { bytes: png, width: px, height: px, size, version: (size - 17) / 4 };
  }

  function qrPngDataUrl(text, opts = {}) {
    const { bytes } = qrPngBytes(text, opts);
    let bin = '';
    for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return 'data:image/png;base64,' + btoa(bin);
  }

  // Deterministic solid black square PNG for 4mm corner registration anchors in Word (.docx).
  function solidBlackSquarePngBytes(px = 32) {
    const stride = 1 + px;
    const raw = new Uint8Array(stride * px);
    raw.fill(0); // filter byte 0 + all black pixels (0x00)
    const ihdr = new Uint8Array(13);
    const dv = new DataView(ihdr.buffer);
    dv.setUint32(0, px, false);
    dv.setUint32(4, px, false);
    ihdr[8] = 8; // 8-bit grayscale
    ihdr[9] = 0;
    const sig = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]);
    const c1 = pngChunk('IHDR', ihdr);
    const c2 = pngChunk('IDAT', zlibStored(raw));
    const c3 = pngChunk('IEND', new Uint8Array(0));
    const png = new Uint8Array(sig.length + c1.length + c2.length + c3.length);
    let p = 0;
    for (const part of [sig, c1, c2, c3]) { png.set(part, p); p += part.length; }
    return png;
  }

  global.qrMatrix = qrMatrix;
  global.qrSvgString = qrSvgString;
  global.qrPngBytes = qrPngBytes;
  global.qrPngDataUrl = qrPngDataUrl;
  global.solidBlackSquarePngBytes = solidBlackSquarePngBytes;
})(typeof window !== 'undefined' ? window : globalThis);
