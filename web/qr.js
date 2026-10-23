// QR codes for share links, shared by the apps hub and the artifacts workspace, so someone in
// the room can scan a link off the screen instead of having it sent to them.
// window.shartifactsQR(text) returns the code as an SVG element, and
// window.shartifactsShowQR(url) shows it large in an overlay over the page (and over any open
// dialog). The encoder is small and has no dependency: byte mode, error correction level M,
// the smallest version that fits, and the mask with the lowest penalty, as ISO/IEC 18004 sets
// out. Level M survives a little glare off a screen and keeps a share link's code small.
(() => {
  // Per version (index 1 to 40) at level M: error correction codewords per block, and blocks.
  const ECC_PER_BLOCK = [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28];
  const BLOCKS = [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49];
  // The two bits that name level M in the format information.
  const LEVEL_M = 0;

  // Modules left for data and error correction once the function patterns are placed.
  function rawModules(ver) {
    let n = (16 * ver + 128) * ver + 64;
    if (ver >= 2) {
      const align = Math.floor(ver / 7) + 2;
      n -= (25 * align - 10) * align - 55;
      if (ver >= 7) n -= 36;
    }
    return n;
  }
  const dataCodewords = (ver) => Math.floor(rawModules(ver) / 8) - ECC_PER_BLOCK[ver] * BLOCKS[ver];

  // Reed-Solomon over GF(256) with the QR polynomial 0x11D.
  function gfMul(x, y) {
    let z = 0;
    for (let i = 7; i >= 0; i--) {
      z = (z << 1) ^ ((z >>> 7) * 0x11d);
      z ^= ((y >>> i) & 1) * x;
    }
    return z;
  }
  function rsDivisor(degree) {
    const r = new Array(degree).fill(0);
    r[degree - 1] = 1;
    let root = 1;
    for (let i = 0; i < degree; i++) {
      for (let j = 0; j < degree; j++) {
        r[j] = gfMul(r[j], root);
        if (j + 1 < degree) r[j] ^= r[j + 1];
      }
      root = gfMul(root, 0x02);
    }
    return r;
  }
  function rsRemainder(data, divisor) {
    const r = divisor.map(() => 0);
    for (const b of data) {
      const factor = b ^ r.shift();
      r.push(0);
      divisor.forEach((d, i) => (r[i] ^= gfMul(d, factor)));
    }
    return r;
  }

  // The data codewords for text in byte mode, padded to fill the version.
  function encodeData(bytes, ver) {
    const bits = [];
    const put = (val, len) => {
      for (let i = len - 1; i >= 0; i--) bits.push((val >>> i) & 1);
    };
    put(0b0100, 4);
    put(bytes.length, ver < 10 ? 8 : 16);
    for (const b of bytes) put(b, 8);
    const capacity = dataCodewords(ver) * 8;
    put(0, Math.min(4, capacity - bits.length));
    put(0, (8 - (bits.length % 8)) % 8);
    for (let pad = 0xec; bits.length < capacity; pad ^= 0xec ^ 0x11) put(pad, 8);
    const out = [];
    for (let i = 0; i < bits.length; i += 8) out.push(bits.slice(i, i + 8).reduce((a, b) => (a << 1) | b, 0));
    return out;
  }

  // Splits the data into blocks, adds each block's error correction, and interleaves them.
  function addEcc(data, ver) {
    const numBlocks = BLOCKS[ver];
    const eccLen = ECC_PER_BLOCK[ver];
    const numShort = numBlocks - (Math.floor(rawModules(ver) / 8) % numBlocks);
    const shortLen = Math.floor(Math.floor(rawModules(ver) / 8) / numBlocks);
    const divisor = rsDivisor(eccLen);
    const blocks = [];
    for (let i = 0, k = 0; i < numBlocks; i++) {
      const dat = data.slice(k, k + shortLen - eccLen + (i < numShort ? 0 : 1));
      k += dat.length;
      const ecc = rsRemainder(dat, divisor);
      // Short blocks hold a placeholder where long blocks have one more data codeword.
      if (i < numShort) dat.push(-1);
      blocks.push(dat.concat(ecc));
    }
    const out = [];
    for (let i = 0; i < blocks[0].length; i++) {
      for (let j = 0; j < blocks.length; j++) {
        if (i !== shortLen - eccLen || j >= numShort) out.push(blocks[j][i]);
      }
    }
    return out;
  }

  function alignmentPositions(ver) {
    if (ver === 1) return [];
    const num = Math.floor(ver / 7) + 2;
    const step = ver === 32 ? 26 : Math.ceil((ver * 4 + 4) / (num * 2 - 2)) * 2;
    const out = [6];
    for (let pos = ver * 4 + 10; out.length < num; pos -= step) out.splice(1, 0, pos);
    return out;
  }

  // BCH-coded bits: 5 data bits of format information, or 6 of version information.
  function formatBits(mask) {
    const data = (LEVEL_M << 3) | mask;
    let rem = data;
    for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
    return ((data << 10) | rem) ^ 0x5412;
  }
  function versionBits(ver) {
    let rem = ver;
    for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
    return (ver << 12) | rem;
  }

  const MASKS = [
    (x, y) => (x + y) % 2 === 0,
    (x, y) => y % 2 === 0,
    (x) => x % 3 === 0,
    (x, y) => (x + y) % 3 === 0,
    (x, y) => (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0,
    (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
    (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0,
    (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
  ];

  // Builds the module grid: true is dark. Rows are y, columns x.
  function build(ver, codewords, mask) {
    const size = ver * 4 + 17;
    const dark = Array.from({ length: size }, () => new Array(size).fill(false));
    const fixed = Array.from({ length: size }, () => new Array(size).fill(false));
    const set = (x, y, on) => {
      dark[y][x] = on;
      fixed[y][x] = true;
    };

    for (let i = 0; i < size; i++) {
      set(6, i, i % 2 === 0);
      set(i, 6, i % 2 === 0);
    }
    for (const [cx, cy] of [[3, 3], [size - 4, 3], [3, size - 4]]) {
      for (let dy = -4; dy <= 4; dy++) {
        for (let dx = -4; dx <= 4; dx++) {
          const x = cx + dx;
          const y = cy + dy;
          const d = Math.max(Math.abs(dx), Math.abs(dy));
          if (x >= 0 && x < size && y >= 0 && y < size) set(x, y, d !== 2 && d !== 4);
        }
      }
    }
    const align = alignmentPositions(ver);
    for (let i = 0; i < align.length; i++) {
      for (let j = 0; j < align.length; j++) {
        // Skip the three corners the finder patterns already hold.
        if ((i === 0 && j === 0) || (i === 0 && j === align.length - 1) || (i === align.length - 1 && j === 0)) continue;
        for (let dy = -2; dy <= 2; dy++) {
          for (let dx = -2; dx <= 2; dx++) set(align[i] + dx, align[j] + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
        }
      }
    }

    const fmt = formatBits(mask);
    const bit = (v, i) => ((v >>> i) & 1) !== 0;
    for (let i = 0; i <= 5; i++) set(8, i, bit(fmt, i));
    set(8, 7, bit(fmt, 6));
    set(8, 8, bit(fmt, 7));
    set(7, 8, bit(fmt, 8));
    for (let i = 9; i < 15; i++) set(14 - i, 8, bit(fmt, i));
    for (let i = 0; i < 8; i++) set(size - 1 - i, 8, bit(fmt, i));
    for (let i = 8; i < 15; i++) set(8, size - 15 + i, bit(fmt, i));
    set(8, size - 8, true);
    if (ver >= 7) {
      const v = versionBits(ver);
      for (let i = 0; i < 18; i++) {
        const a = size - 11 + (i % 3);
        const b = Math.floor(i / 3);
        set(a, b, bit(v, i));
        set(b, a, bit(v, i));
      }
    }

    // Data runs in two-column strips from the bottom right, zigzagging up and down.
    let i = 0;
    for (let right = size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5;
      for (let vert = 0; vert < size; vert++) {
        for (let j = 0; j < 2; j++) {
          const x = right - j;
          const upward = ((right + 1) & 2) === 0;
          const y = upward ? size - 1 - vert : vert;
          if (fixed[y][x]) continue;
          const on = i < codewords.length * 8 && bit(codewords[i >>> 3], 7 - (i & 7));
          i++;
          dark[y][x] = on !== MASKS[mask](x, y);
        }
      }
    }
    return dark;
  }

  // The standard's penalty score: lower reads more reliably.
  function penalty(dark) {
    const size = dark.length;
    let score = 0;
    const lines = [];
    for (let y = 0; y < size; y++) lines.push(dark[y]);
    for (let x = 0; x < size; x++) lines.push(dark.map((row) => row[x]));
    for (const line of lines) {
      let run = 1;
      for (let i = 1; i <= size; i++) {
        if (i < size && line[i] === line[i - 1]) run++;
        else {
          if (run >= 5) score += run - 2;
          run = 1;
        }
      }
      // A finder-like 1:1:3:1:1 pattern with four light modules on one side; the quiet
      // zone around the code counts as light.
      const s = `0000${line.map((d) => (d ? "1" : "0")).join("")}0000`;
      for (const p of ["00001011101", "10111010000"]) {
        for (let i = s.indexOf(p); i !== -1; i = s.indexOf(p, i + 1)) score += 40;
      }
    }
    let blacks = 0;
    for (let y = 0; y < size; y++) {
      for (let x = 0; x < size; x++) {
        if (dark[y][x]) blacks++;
        if (x < size - 1 && y < size - 1) {
          const c = dark[y][x];
          if (c === dark[y][x + 1] && c === dark[y + 1][x] && c === dark[y + 1][x + 1]) score += 3;
        }
      }
    }
    const total = size * size;
    score += (Math.ceil(Math.abs(blacks * 20 - total * 10) / total) - 1) * 10;
    return score;
  }

  function encode(text) {
    const bytes = new TextEncoder().encode(text);
    let ver = 1;
    while (ver <= 40 && 4 + (ver < 10 ? 8 : 16) + bytes.length * 8 > dataCodewords(ver) * 8) ver++;
    if (ver > 40) throw new Error("too long for a QR code");
    const codewords = addEcc(encodeData(bytes, ver), ver);
    let best = null;
    let bestScore = Infinity;
    for (let mask = 0; mask < 8; mask++) {
      const grid = build(ver, codewords, mask);
      const score = penalty(grid);
      if (score < bestScore) [best, bestScore] = [grid, score];
    }
    return best;
  }

  // One path of unit squares inside a four-module quiet zone; it scales to any size.
  function svg(text) {
    const dark = encode(text);
    const ns = "http://www.w3.org/2000/svg";
    const n = dark.length + 8;
    const el = document.createElementNS(ns, "svg");
    el.setAttribute("viewBox", `0 0 ${n} ${n}`);
    el.setAttribute("shape-rendering", "crispEdges");
    el.setAttribute("role", "img");
    el.setAttribute("aria-label", `QR code for ${text}`);
    const bg = document.createElementNS(ns, "rect");
    bg.setAttribute("width", n);
    bg.setAttribute("height", n);
    bg.setAttribute("fill", "#fff");
    let d = "";
    dark.forEach((row, y) => row.forEach((on, x) => on && (d += `M${x + 4} ${y + 4}h1v1h-1z`)));
    const path = document.createElementNS(ns, "path");
    path.setAttribute("d", d);
    path.setAttribute("fill", "#000");
    el.append(bg, path);
    return el;
  }
  window.shartifactsQR = svg;

  // A modal dialog joins the top layer above any dialog already open, such as the share panel
  // the button sits in. Any click or Escape closes it.
  let overlay = null;
  window.shartifactsShowQR = (url) => {
    if (!overlay) {
      overlay = document.createElement("dialog");
      overlay.className = "qr-overlay";
      overlay.onclick = () => overlay.close();
      overlay.addEventListener("close", () => overlay.replaceChildren());
      document.body.append(overlay);
    }
    const code = document.createElement("div");
    code.className = "qr-code";
    code.append(svg(url));
    const caption = document.createElement("p");
    caption.className = "qr-url";
    caption.textContent = url;
    const hint = document.createElement("p");
    hint.className = "qr-hint";
    hint.textContent = "Scan with a phone camera. Click or press Esc to close.";
    overlay.replaceChildren(code, caption, hint);
    overlay.showModal();
  };
})();
