// The login page: sends the password, then returns to the page that sent the browser here.
(() => {
  const form = document.getElementById("login-form");
  const error = document.getElementById("login-error");
  // Only ever back to a page of this server: a path in printable ASCII without a backslash,
  // because browsers drop tabs and newlines from URLs and read a backslash as a slash, which
  // could otherwise turn "/" plus something into "//elsewhere".
  const next = (() => {
    const asked = new URLSearchParams(location.search).get("next") || "/";
    if (!/^\/(?!\/)[\x21-\x7e]*$/.test(asked) || asked.includes("\\")) return "/";
    try {
      const u = new URL(asked, location.origin);
      return u.origin === location.origin ? u.pathname + u.search + u.hash : "/";
    } catch {
      return "/";
    }
  })();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const go = form.querySelector('button[type="submit"]');
    go.disabled = true;
    error.textContent = "";
    try {
      const r = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: form.password.value }),
      });
      const body = await r.json().catch(() => ({}));
      if (r.ok || body.code === "sign_in_off") {
        location.replace(next);
        return;
      }
      error.textContent = body.code === "wrong_password" ? "That isn't the password." : body.error || `Sign-in failed (${r.status}).`;
      form.password.select();
    } catch (err) {
      error.textContent = `Can't reach shartifacts: ${err.message}`;
    } finally {
      go.disabled = false;
    }
  });
})();
