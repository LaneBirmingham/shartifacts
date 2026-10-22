// KaTeX for Markdown and slide previews: `$…$` and `\(…\)` inline, `$$…$$` and `\[…\]`
// display. A `$` needs text right after it and no digit right after its closing `$`, so
// prices like $5 and $10 stay text.
import katex from "katex";

const render = (text, displayMode) =>
  katex.renderToString(text, { displayMode, throwOnError: false });
const block = {
  name: "mathBlock",
  level: "block",
  start(src) {
    const i = src.search(/^(\$\$|\\\[)/m);
    return i < 0 ? undefined : i;
  },
  tokenizer(src) {
    const m = /^(?:\$\$([\s\S]+?)\$\$|\\\[([\s\S]+?)\\\])[ \t]*(?:\n|$)/.exec(src);
    if (m) return { type: "mathBlock", raw: m[0], text: m[1] ?? m[2] };
  },
  renderer: (t) => `<p class="math-display">${render(t.text, true)}</p>`,
};
const inline = {
  name: "math",
  level: "inline",
  start(src) {
    const i = src.search(/\$|\\[([]/);
    return i < 0 ? undefined : i;
  },
  tokenizer(src) {
    let m = /^\$\$([\s\S]+?)\$\$/.exec(src) || /^\\\[([\s\S]+?)\\\]/.exec(src);
    if (m) return { type: "math", raw: m[0], text: m[1], display: true };
    m = /^\\\(([\s\S]+?)\\\)/.exec(src) || /^\$(?=\S)((?:\\.|[^$\\\n])*?\S)\$(?!\d)/.exec(src);
    if (m) return { type: "math", raw: m[0], text: m[1], display: false };
  },
  renderer: (t) => render(t.text, t.display),
};
export const marked = { extensions: [block, inline] };
export default katex;
