import { build as esbuild } from "esbuild";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { copyFile, mkdir, readFile, rm, writeFile } from "node:fs/promises";
// Every npm package that ends up in a bundle, so THIRD_PARTY_NOTICES.txt can carry each
// one's license. esbuild only keeps the license comments a library writes into its
// source, and d3, highlight.js and KaTeX (among others) write none.
const bundled = new Set();
async function build(options) {
  const result = await esbuild({ ...options, metafile: true });
  for (const input of Object.keys(result.metafile.inputs)) {
    const m = input.match(/^(.*node_modules\/(?:@[^/]+\/)?[^/]+)\//);
    if (m) bundled.add(m[1]);
  }
  return result;
}
await rm("web/vendor/lib", { recursive: true, force: true });
await mkdir("web/vendor/lib", { recursive: true });
// One React for the page. Every library bundle below and every compiled artifact reads
// these globals, so hooks and context are shared across them.
await build({
  stdin: {
    contents: `import React from 'react'; import ReactDOM from 'react-dom'; import ReactDOMClient from 'react-dom/client'; import JSXRuntime from 'react/jsx-runtime';
window.React = React; window.ReactDOM = ReactDOM; window.ReactDOMClient = ReactDOMClient; window.ReactJSXRuntime = JSXRuntime; window.createRoot = ReactDOMClient.createRoot;`,
    resolveDir: process.cwd(),
  },
  bundle: true,
  minify: true,
  format: "iife",
  outfile: "web/vendor/react.js",
  define: { "process.env.NODE_ENV": '"production"' },
  legalComments: "linked",
});
await build({
  stdin: {
    contents: `import {marked} from 'marked'; import mermaid from 'mermaid'; window.marked = marked; window.mermaid = mermaid;`,
    resolveDir: process.cwd(),
  },
  bundle: true,
  minify: true,
  format: "iife",
  outfile: "web/vendor/documents.js",
  legalComments: "linked",
});

// KaTeX's stylesheet with its woff2 fonts inlined: previews only load fonts from data: URLs.
const katexCSS = (await readFile("node_modules/katex/dist/katex.min.css", "utf8")).replace(
  /src:url\(fonts\/([\w-]+)\.woff2\) format\("woff2"\)[^;}]*/g,
  (_, font) => `src:url(data:font/woff2;base64,${readFileSync(`node_modules/katex/dist/fonts/${font}.woff2`).toString("base64")}) format("woff2")`,
);

// Libraries an artifact can import (React) or load with a script tag (HTML). Each is
// built separately and loaded only by artifacts that use it. `esm` libraries are stored
// as namespaces; CommonJS ones as their module.exports. `global` is the name the library's
// own UMD build uses, so CDN-style HTML works unchanged. Keep in sync with
// internal/server/libraries.go.
const libraries = {
  "lucide-react": { esm: "lucide-react" },
  recharts: { esm: "recharts", global: "Recharts" },
  d3: { esm: "d3", global: "d3" },
  mathjs: { esm: "mathjs", global: "math" },
  three: { esm: "three", global: "THREE" },
  xlsx: { esm: "xlsx", global: "XLSX" },
  tone: { esm: "tone", global: "Tone" },
  chart: {
    contents: `import Chart from 'chart.js/auto'; import * as m from 'chart.js'; export const lib = {...m, default: Chart, __esModule: true}; export const global = Chart;`,
    global: "Chart",
  },
  lodash: { cjs: "lodash", global: "_" },
  papaparse: { cjs: "papaparse", global: "Papa" },
  plotly: { cjs: "plotly.js-dist-min", global: "Plotly" },
  // Math for Markdown and slides (marked extension) and for artifacts importing katex.
  katex: {
    contents: `import katex, {marked} from './scripts/math.js';
const style = document.createElement('style'); style.textContent = ${JSON.stringify(katexCSS)}; (document.head || document.documentElement).append(style);
window.__shartifactsMath = marked;
export const lib = {...katex, default: katex, __esModule: true}; export const global = katex;`,
    global: "katex",
  },
  // Syntax highlighting for code and Markdown previews; brings its own light theme.
  highlight: {
    contents: `import hljs from 'highlight.js/lib/common'; import css from 'highlight.js/styles/github.min.css';
const style = document.createElement('style'); style.textContent = css + 'pre code.hljs{padding:0;background:none}'; (document.head || document.documentElement).append(style);
export const lib = hljs; export const global = hljs;`,
    global: "hljs",
  },
};
const reactGlobals = {
  react: "React",
  "react-dom": "ReactDOM",
  "react-dom/client": "ReactDOMClient",
  "react/jsx-runtime": "ReactJSXRuntime",
  "react/jsx-dev-runtime": "ReactJSXRuntime",
};
const sharedReact = {
  name: "shared-react",
  setup(b) {
    b.onResolve({ filter: /^react(-dom)?(\/.*)?$/ }, (a) =>
      reactGlobals[a.path] ? { path: a.path, namespace: "react-global" } : undefined,
    );
    b.onLoad({ filter: /.*/, namespace: "react-global" }, (a) => ({
      contents: `module.exports = window.${reactGlobals[a.path]};`,
      loader: "js",
    }));
  },
};
for (const [name, lib] of Object.entries(libraries)) {
  const value = lib.contents
    ? lib.contents
    : lib.esm
      ? `import * as m from '${lib.esm}'; export const lib = {...m, __esModule: true}; export const global = m;`
      : `import m from '${lib.cjs}'; export const lib = m; export const global = m;`;
  await build({
    stdin: {
      contents: `${value}
(window.__shartifactsLib ||= {})[${JSON.stringify(name)}] = lib;${lib.global ? `\nwindow[${JSON.stringify(lib.global)}] ||= global;` : ""}`,
      resolveDir: process.cwd(),
    },
    bundle: true,
    minify: true,
    format: "iife",
    outfile: `web/vendor/lib/${name}.js`,
    loader: { ".css": "text" },
    define: { "process.env.NODE_ENV": '"production"', global: "window" },
    plugins: [sharedReact],
    legalComments: "linked",
    logLevel: "warning",
  });
}
// shadcn/ui-compatible components for `@/components/ui/*` imports.
await build({
  entryPoints: ["scripts/ui.jsx"],
  bundle: true,
  minify: true,
  format: "iife",
  globalName: "__shartifactsUI",
  footer: { js: `(window.__shartifactsLib ||= {}).ui = {...__shartifactsUI, __esModule: true};` },
  jsx: "automatic",
  outfile: "web/vendor/lib/ui.js",
  plugins: [sharedReact],
  logLevel: "warning",
});
// The design system's open-source type: Inter, Cormorant Garamond (its documented 500
// cut) for display, and JetBrains Mono. Latin subsets; design-system.css declares them.
await rm("web/vendor/fonts", { recursive: true, force: true });
await mkdir("web/vendor/fonts", { recursive: true });
for (const [from, to] of [
  ["@fontsource-variable/inter/files/inter-latin-wght-normal.woff2", "inter.woff2"],
  ["@fontsource/cormorant-garamond/files/cormorant-garamond-latin-500-normal.woff2", "cormorant-garamond-500.woff2"],
  ["@fontsource/cormorant-garamond/files/cormorant-garamond-latin-500-italic.woff2", "cormorant-garamond-500-italic.woff2"],
  ["@fontsource-variable/jetbrains-mono/files/jetbrains-mono-latin-wght-normal.woff2", "jetbrains-mono.woff2"],
])
  await copyFile(`node_modules/${from}`, `web/vendor/fonts/${to}`);
for (const pkg of ["@fontsource-variable/inter", "@fontsource/cormorant-garamond", "@fontsource-variable/jetbrains-mono"])
  await copyFile(`node_modules/${pkg}/LICENSE`, `web/vendor/fonts/${pkg.split("/")[1]}.LICENSE.txt`);
// Tailwind's browser build generates utility CSS for the classes in the page.
await writeFile(
  "web/vendor/lib/tailwind.js",
  (await readFile("scripts/tailwind-compat.js", "utf8")) +
    (await readFile("node_modules/@tailwindcss/browser/dist/index.global.js", "utf8")),
);
await copyFile(
  "node_modules/@tailwindcss/browser/LICENSE",
  "web/vendor/lib/tailwind.js.LEGAL.txt",
);

function mit(holder, year) {
  return `MIT License

Copyright (c) ${year ? `${year} ` : ""}${holder}

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.`;
}
// One file with the license of everything above, in the binary next to the bundles.
// shadcn/ui is not an npm dependency: scripts/ui.jsx follows its component API and classes.
bundled.add("node_modules/@tailwindcss/browser");
for (const pkg of ["@fontsource-variable/inter", "@fontsource/cormorant-garamond", "@fontsource-variable/jetbrains-mono"])
  bundled.add(`node_modules/${pkg}`);
const notices = [...bundled]
  .map((dir) => {
    const { name, version, license, author } = JSON.parse(readFileSync(`${dir}/package.json`, "utf8"));
    const file = readdirSync(dir).find((f) => /^(licen[cs]e|copying)(\.(md|txt))?$/i.test(f));
    if (file) return { name, version, license, text: readFileSync(`${dir}/${file}`, "utf8").trim() };
    // victory-vendor (in recharts) ships no license of its own, only those of the d3
    // modules it repackages; its package.json declares MIT for its own code.
    if (existsSync(`${dir}/lib-vendor`)) {
      const vendored = readdirSync(`${dir}/lib-vendor`)
        .filter((d) => existsSync(`${dir}/lib-vendor/${d}/LICENSE`))
        .map((d) => `--- ${d} ---\n\n${readFileSync(`${dir}/lib-vendor/${d}/LICENSE`, "utf8").trim()}`);
      return { name, version, license, text: [`${name}'s own code: ${license} (see its package.json).`, ...vendored].join("\n\n") };
    }
    // seedrandom and javascript-natural-sort (both in mathjs) declare MIT in
    // package.json without shipping the text: give it with their author.
    if (license === "MIT") return { name, version, license, text: mit(typeof author === "string" ? author : author?.name) };
    throw new Error(`${name} has no license file; add its notice by hand`);
  })
  .filter((n, i, all) => all.findIndex((o) => o.name === n.name && o.version === n.version) === i)
  .sort((a, b) => a.name.localeCompare(b.name) || a.version.localeCompare(b.version));
notices.push({
  name: "shadcn/ui (component API and classes followed by web/vendor/lib/ui.js)",
  license: "MIT",
  text: mit("shadcn", "2023"),
});
await writeFile(
  "web/vendor/THIRD_PARTY_NOTICES.txt",
  "Third-party software bundled in web/vendor/ (generated by scripts/build.mjs).\n\n" +
    notices
      .map((n) => `${"=".repeat(78)}\n${n.name}${n.version ? `@${n.version}` : ""} (${n.license ?? "see below"})\n${"=".repeat(78)}\n\n${n.text}\n`)
      .join("\n"),
);
