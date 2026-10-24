// Minimal ANSI SGR -> styled segments converter for log lines.

export interface Segment {
  text: string;
  cls?: string;
  style?: { color?: string; backgroundColor?: string };
}

interface State {
  fg?: number | string; // 0-15 palette index or css colour
  bg?: number | string;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
  inverse?: boolean;
  strike?: boolean;
}

// Matches CSI sequences (ESC [ ... final byte), OSC (ESC ] ... BEL/ST) and lone two-byte escapes.
// eslint-disable-next-line no-control-regex
const ESC_RE = /\x1b\[([0-9;:?]*)([@-~])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]/g;

function color256(n: number): number | string {
  if (n < 16) return n;
  if (n < 232) {
    n -= 16;
    const levels = [0, 95, 135, 175, 215, 255];
    const r = levels[Math.floor(n / 36)];
    const g = levels[Math.floor(n / 6) % 6];
    const b = levels[n % 6];
    return `rgb(${r},${g},${b})`;
  }
  const v = 8 + (n - 232) * 10;
  return `rgb(${v},${v},${v})`;
}

function applySGR(state: State, params: string): State {
  const codes = params === "" ? [0] : params.split(/[;:]/).map((x) => (x === "" ? 0 : parseInt(x, 10)));
  const s: State = { ...state };
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i];
    if (c === 0) {
      for (const k of Object.keys(s)) delete (s as Record<string, unknown>)[k];
    } else if (c === 1) s.bold = true;
    else if (c === 2) s.dim = true;
    else if (c === 3) s.italic = true;
    else if (c === 4) s.underline = true;
    else if (c === 7) s.inverse = true;
    else if (c === 9) s.strike = true;
    else if (c === 22) {
      s.bold = false;
      s.dim = false;
    } else if (c === 23) s.italic = false;
    else if (c === 24) s.underline = false;
    else if (c === 27) s.inverse = false;
    else if (c === 29) s.strike = false;
    else if (c >= 30 && c <= 37) s.fg = c - 30;
    else if (c === 39) s.fg = undefined;
    else if (c >= 40 && c <= 47) s.bg = c - 40;
    else if (c === 49) s.bg = undefined;
    else if (c >= 90 && c <= 97) s.fg = c - 90 + 8;
    else if (c >= 100 && c <= 107) s.bg = c - 100 + 8;
    else if (c === 38 || c === 48) {
      let col: number | string | undefined;
      if (codes[i + 1] === 5 && i + 2 < codes.length) {
        col = color256(codes[i + 2]);
        i += 2;
      } else if (codes[i + 1] === 2 && i + 4 < codes.length) {
        col = `rgb(${codes[i + 2]},${codes[i + 3]},${codes[i + 4]})`;
        i += 4;
      }
      if (c === 38) s.fg = col;
      else s.bg = col;
    }
  }
  return s;
}

function toSegment(text: string, st: State): Segment {
  const cls: string[] = [];
  const style: Segment["style"] = {};
  let fg = st.fg;
  let bg = st.bg;
  if (st.inverse) {
    [fg, bg] = [bg ?? "fg", fg ?? "bg"];
  }
  if (fg === "fg") cls.push("ansi-inv-fg");
  else if (typeof fg === "number") cls.push(`ansi-fg-${fg}`);
  else if (typeof fg === "string") style.color = fg;
  if (bg === "bg") cls.push("ansi-inv-bg");
  else if (typeof bg === "number") cls.push(`ansi-bg-${bg}`);
  else if (typeof bg === "string") style.backgroundColor = bg;
  if (st.bold) cls.push("ansi-bold");
  if (st.dim) cls.push("ansi-dim");
  if (st.italic) cls.push("ansi-italic");
  if (st.underline) cls.push("ansi-underline");
  if (st.strike) cls.push("ansi-strike");
  const seg: Segment = { text };
  if (cls.length) seg.cls = cls.join(" ");
  if (style.color || style.backgroundColor) seg.style = style;
  return seg;
}

/** Convert one line with ANSI escapes into styled segments. */
export function ansiToSegments(input: string): Segment[] {
  if (input.indexOf("\x1b") < 0) return [{ text: input }];
  const out: Segment[] = [];
  let state: State = {};
  let last = 0;
  ESC_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = ESC_RE.exec(input))) {
    if (m.index > last) out.push(toSegment(input.slice(last, m.index), state));
    if (m[2] === "m") state = applySGR(state, m[1] ?? "");
    last = ESC_RE.lastIndex;
  }
  if (last < input.length) out.push(toSegment(input.slice(last), state));
  return out;
}

/** Remove all ANSI escapes. */
export function stripAnsi(input: string): string {
  return input.replace(ESC_RE, "");
}
