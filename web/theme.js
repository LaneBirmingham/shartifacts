// Light or dark: the saved choice, else the system's. Loaded in the head so the page
// paints in the right theme; every [data-theme-toggle] button flips it and saves it.
(() => {
  const root = document.documentElement;
  const system = matchMedia("(prefers-color-scheme: dark)");
  const colors = { light: "#faf9f5", dark: "#1c1b19" };
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
  let choice = saved();
  function apply() {
    const theme = choice || (system.matches ? "dark" : "light");
    root.dataset.theme = theme;
    for (const meta of document.querySelectorAll('meta[name="theme-color"]')) meta.content = colors[theme];
    for (const button of document.querySelectorAll("[data-theme-toggle]")) {
      button.innerHTML = icons[theme];
      button.title = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";
      button.setAttribute("aria-label", button.title);
    }
  }
  document.addEventListener("click", (e) => {
    if (!e.target.closest?.("[data-theme-toggle]")) return;
    choice = root.dataset.theme === "dark" ? "light" : "dark";
    try {
      localStorage.setItem("shartifacts-theme", choice);
    } catch {}
    apply();
  });
  system.addEventListener("change", apply);
  addEventListener("storage", (e) => {
    if (e.key !== "shartifacts-theme") return;
    choice = saved();
    apply();
  });
  apply();
  document.addEventListener("DOMContentLoaded", apply);
})();
