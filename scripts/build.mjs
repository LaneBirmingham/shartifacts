import { build } from "esbuild";
import { readFileSync } from "node:fs";
import { copyFile, mkdir, readFile, rm, writeFile } from "node:fs/promises";
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
// The design system's open-source type: Inter for Copernicus' sans partner StyreneB,
// Cormorant Garamond (its documented 500 cut) for the Copernicus display serif, and
// JetBrains Mono. Latin subsets; design-system.css declares them.
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
