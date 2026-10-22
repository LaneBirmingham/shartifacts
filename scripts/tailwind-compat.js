// Prepended to Tailwind's v4 browser build. Artifacts are usually written for the v3 Play
// CDN: restore the v3 defaults v4 changed, and accept `tailwind.config = {...}` by turning
// its colors and fonts into v4 theme variables.
(() => {
  const style = (css) => {
    const s = document.createElement("style");
    s.type = "text/tailwindcss";
    s.textContent = css;
    (document.head || document.documentElement).append(s);
  };
  style(
    `@layer base { *, ::before, ::after { border-color: var(--color-gray-200, currentColor); } button:not(:disabled), [role="button"]:not(:disabled) { cursor: pointer; } }`,
  );
  let config = {};
  const vars = (prefix, values) =>
    Object.entries(values || {})
      .flatMap(([name, value]) =>
        typeof value === "object" && !Array.isArray(value)
          ? Object.entries(value).map(([shade, v]) =>
              shade === "DEFAULT" ? `--${prefix}-${name}: ${v};` : `--${prefix}-${name}-${shade}: ${v};`,
            )
          : [`--${prefix}-${name}: ${[].concat(value).join(", ")};`],
      )
      .join(" ");
  window.tailwind = {
    get config() {
      return config;
    },
    set config(c) {
      config = c || {};
      const theme = { ...(config.theme || {}), ...(config.theme?.extend || {}) };
      const css = vars("color", theme.colors) + " " + vars("font", theme.fontFamily);
      if (css.trim()) style(`@theme { ${css} }`);
    },
  };
})();
