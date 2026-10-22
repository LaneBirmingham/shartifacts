// Light or dark: the saved choice, else the system's. Loaded in the head so the page
// paints in the right theme; every [data-theme-toggle] button flips it and saves it.
// The palette (palettes.css) is a separate choice, made in any [data-palette-picker].
// Both are saved per browser and followed live by the browser's other tabs.
(() => {
  const root = document.documentElement;
  const system = matchMedia("(prefers-color-scheme: dark)");
  // Each palette's canvas, for the browser chrome (theme-color).
  const palettes = {
    terracotta: { label: "Terracotta", light: "#f9f8f6", dark: "#1c1a19" },
    harbour: { label: "Harbour", light: "#f8f7f3", dark: "#181a1e" },
    sage: { label: "Sage", light: "#f8f7f2", dark: "#191b19" },
  };
  const icons = {
    // Shown in light mode: switch to dark.
    light: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/></svg>',
    dark: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>',
  };
  const saved = () => {
    try {
      const t = localStorage.getItem("shartifacts-theme");
      return t === "light" || t === "dark" ? t : null;
    } catch {
      return null;
    }
  };
  const savedPalette = () => {
    try {
      const p = localStorage.getItem("shartifacts-palette");
      return Object.hasOwn(palettes, p) ? p : "terracotta";
    } catch {
      return "terracotta";
    }
  };
  let choice = saved();
  let palette = savedPalette();
  function apply() {
    const theme = choice || (system.matches ? "dark" : "light");
    root.dataset.theme = theme;
    root.dataset.palette = palette;
    for (const meta of document.querySelectorAll('meta[name="theme-color"]')) meta.content = palettes[palette][theme];
    for (const picker of document.querySelectorAll("[data-palette-picker]")) {
      if (!picker.childElementCount)
        for (const [id, p] of Object.entries(palettes)) {
          const button = document.createElement("button");
          button.type = "button";
          button.dataset.paletteChoice = id;
          button.innerHTML = '<i aria-hidden="true"></i>';
          button.append(p.label);
          picker.append(button);
        }
      for (const button of picker.querySelectorAll("[data-palette-choice]"))
        button.setAttribute("aria-pressed", String(button.dataset.paletteChoice === palette));
    }
    for (const button of document.querySelectorAll("[data-theme-toggle]")) {
      button.innerHTML = icons[theme];
      button.title = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";
      button.setAttribute("aria-label", button.title);
    }
  }
  document.addEventListener("click", (e) => {
    const pick = e.target.closest?.("[data-palette-choice]");
    if (pick) {
      palette = pick.dataset.paletteChoice;
      try {
        localStorage.setItem("shartifacts-palette", palette);
      } catch {}
      apply();
      return;
    }
    if (!e.target.closest?.("[data-theme-toggle]")) return;
    choice = root.dataset.theme === "dark" ? "light" : "dark";
    try {
      localStorage.setItem("shartifacts-theme", choice);
    } catch {}
    apply();
  });
  system.addEventListener("change", apply);
  addEventListener("storage", (e) => {
    if (e.key === "shartifacts-theme") choice = saved();
    else if (e.key === "shartifacts-palette") palette = savedPalette();
    else return;
    apply();
  });
  apply();
  document.addEventListener("DOMContentLoaded", apply);
})();
