// Sign-in, shared by the workspace's pages (the login page has its own login.js).
//
// Once a password is set, an API answer of 401 means this browser's session has ended, so
// the page goes to the login page and comes back after. Any [data-signin] element opens the
// Sign-in dialog, where people set or change the password, sign out, and see, make and
// revoke agent tokens. window.shartifactsAuth shares the state with connect.js.
(() => {
  const realFetch = window.fetch.bind(window);
  const toLogin = () => location.assign(`/login?next=${encodeURIComponent(location.pathname + location.search + location.hash)}`);
  window.fetch = async (input, init) => {
    const r = await realFetch(input, init);
    if (r.status === 401) {
      const url = new URL(input instanceof Request ? input.url : String(input), location.href);
      if (url.origin === location.origin && url.pathname.startsWith("/api/") && !url.pathname.startsWith("/api/auth")) {
        const body = await r
          .clone()
          .json()
          .catch(() => null);
        if (body?.code === "unauthenticated") toLogin();
      }
    }
    return r;
  };

  let state = { enabled: false, signed_in: false, via: "" };
  let ready;
  async function refresh() {
    try {
      state = await (await realFetch("/api/auth")).json();
    } catch {}
    paint();
    return state;
  }
  ready = refresh();
  window.shartifactsAuth = { ready: () => ready, refresh, get state() { return state; } };

  async function api(path, method = "GET", body) {
    const r = await realFetch(path, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
    const out = await r.json().catch(() => ({}));
    if (!r.ok) throw Object.assign(new Error(out.error || `Request failed (${r.status})`), { code: out.code });
    return out;
  }

  const icons = {
    on: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/></svg>',
    off: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 7.5-2"/></svg>',
  };
  // Buttons show whether sign-in is on: icon buttons by their lock, labelled ones in words.
  function paint() {
    for (const b of document.querySelectorAll("[data-signin]")) {
      const label = state.enabled ? "Sign-in and agent tokens" : "Sign-in is off: set a password";
      if (b.classList.contains("icon-button")) {
        b.innerHTML = icons[state.enabled ? "on" : "off"];
        b.title = label;
        b.setAttribute("aria-label", label);
      } else {
        b.textContent = state.enabled ? "⚿ Sign-in & tokens" : "⚿ Set a password";
      }
    }
  }
  document.addEventListener("DOMContentLoaded", paint);

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  };
  const button = (text, cls, onclick) => {
    const b = el("button", cls, text);
    b.type = "button";
    if (onclick) b.onclick = onclick;
    return b;
  };
  const field = (label, name, autocomplete) => {
    const l = el("label", "", label);
    const i = el("input");
    i.type = "password";
    i.name = name;
    i.autocomplete = autocomplete;
    i.required = true;
    l.append(i);
    return l;
  };
  // A form that runs act(values) and shows its error; the hidden username helps password
  // managers file the password under this server.
  function form(fields, submit, act, cls = "primary") {
    const f = el("form", "signin-form");
    const user = el("input", "visually-hidden");
    user.name = "username";
    user.autocomplete = "username";
    user.value = "owner";
    user.tabIndex = -1;
    user.setAttribute("aria-hidden", "true");
    const go = el("button", cls, submit);
    go.type = "submit";
    const error = el("p", "form-error");
    error.setAttribute("role", "alert");
    f.append(user, ...fields, go, error);
    f.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      go.disabled = true;
      try {
        await act(Object.fromEntries(new FormData(f)));
      } catch (err) {
        error.textContent = err.code === "wrong_password" ? "That isn't the current password." : err.message;
      } finally {
        go.disabled = false;
      }
    });
    return f;
  }
  const twice = (v) => {
    if (v.password !== v.again) throw new Error("The two new passwords differ.");
  };

  let dialog;
  function open() {
    if (!dialog) {
      dialog = el("dialog", "signin-dialog");
      dialog.id = "signin-dialog";
      dialog.setAttribute("aria-labelledby", "signin-title");
      dialog.addEventListener("click", (e) => {
        if (e.target === dialog) dialog.close();
      });
      document.body.append(dialog);
    }
    render();
    dialog.showModal();
  }
  document.addEventListener("click", (e) => {
    if (e.target.closest?.("[data-signin]")) open();
  });

  async function render() {
    const head = el("div", "dialog-heading");
    const close = button("×", "", () => dialog.close());
    close.dataset.close = "";
    close.setAttribute("aria-label", "Close");
    head.append(el("span", "eyebrow", "SIGN-IN"), close);
    const title = el("h2");
    title.id = "signin-title";
    const body = el("div", "signin-body");
    dialog.replaceChildren(head, title, body);
    await refresh();

    if (!state.enabled) {
      title.textContent = "Anyone on your network can use this.";
      body.append(
        el("p", "", "With a password, browsers sign in once and agents connect with tokens you can revoke. Share and app links stay public."),
        form([field("New password (8 characters or more)", "password", "new-password"), field("Again", "again", "new-password")], "Turn on sign-in", async (v) => {
          twice(v);
          await api("/api/auth/password", "PUT", { password: v.password });
          render();
        }),
        el("p", "fineprint", "Agents already connected will need a token: make one with Connect an agent. If you forget the password, shartifacts auth set-password on the server sets a new one."),
      );
      return;
    }

    title.textContent = "Sign-in is on.";
    const session = state.via === "session";
    body.append(el("p", "", session ? "This browser is signed in. Agents connect with tokens, each of which can read and change everything here until you revoke it." : "Signed in with an agent token."));
    if (!session) return;
    const out = el("div", "signin-actions");
    out.append(
      button("Sign out", "quiet", async () => {
        await api("/api/auth/logout", "POST");
        location.assign("/login");
      }),
      button("Sign out everywhere", "quiet", async () => {
        if (!confirm("Sign out every browser, including this one? Agent tokens keep working.")) return;
        await api("/api/auth/logout", "POST", { everywhere: true });
        location.assign("/login");
      }),
    );
    body.append(out);

    const tokens = el("section", "signin-section");
    tokens.append(el("h3", "", "Agent tokens"));
    const list = el("ul", "token-list");
    const made = el("div", "token-made");
    const listTokens = async () => {
      const { tokens: all } = await api("/api/auth/tokens");
      list.replaceChildren();
      if (!all.length) list.append(el("li", "fineprint", "No tokens yet. Connect an agent makes one with the commands to use it."));
      for (const t of all) {
        const li = el("li");
        const text = el("div");
        const when = new Date(t.created_at).toLocaleDateString();
        const used = t.last_used_at ? `last used ${new Date(t.last_used_at).toLocaleString()}` : "never used";
        text.append(el("strong", "", t.name), el("small", "", `${t.hint}… · made ${when} · ${used}`));
        li.append(
          text,
          button("Revoke", "quiet danger", async () => {
            if (!confirm(`Revoke “${t.name}”? Agents using it are refused from their next request.`)) return;
            await api(`/api/auth/tokens/${encodeURIComponent(t.id)}`, "DELETE");
            listTokens();
          }),
        );
        list.append(li);
      }
    };
    const name = el("input");
    name.name = "name";
    name.placeholder = "Name, e.g. Codex on my laptop";
    name.maxLength = 60;
    name.required = true;
    const make = el("form", "token-form");
    const makeButton = el("button", "quiet", "Make a token");
    makeButton.type = "submit";
    const makeError = el("p", "form-error");
    make.append(name, makeButton);
    make.addEventListener("submit", async (e) => {
      e.preventDefault();
      makeError.textContent = "";
      try {
        const t = await api("/api/auth/tokens", "POST", { name: name.value });
        name.value = "";
        const row = el("div", "copy-row");
        const shown = el("input");
        shown.readOnly = true;
        shown.value = t.token;
        const copy = button("Copy", "quiet", async () => {
          copy.textContent = (await window.shartifactsCopy?.(t.token)) ? "Copied" : "Select and copy";
        });
        row.append(shown, copy);
        made.replaceChildren(el("p", "fineprint", `“${t.name}”: copy it now; it isn't shown again. Agents send it as Authorization: Bearer <token>.`), row);
        listTokens();
      } catch (err) {
        makeError.textContent = err.message;
      }
    });
    tokens.append(list, make, makeError, made);
    body.append(tokens);
    listTokens().catch((err) => list.replaceChildren(el("li", "form-error", err.message)));

    const change = el("details", "signin-section");
    const revoke = el("label", "check");
    const revokeBox = el("input");
    revokeBox.type = "checkbox";
    revokeBox.name = "revoke_tokens";
    revoke.append(revokeBox, " Also revoke every agent token (if someone else may know the old password)");
    change.append(
      el("summary", "", "Change the password"),
      form([field("Current password", "current", "current-password"), field("New password", "password", "new-password"), field("Again", "again", "new-password"), revoke], "Change password", async (v) => {
        twice(v);
        await api("/api/auth/password", "PUT", { current: v.current, password: v.password, revoke_tokens: v.revoke_tokens === "on" });
        render();
      }),
      el("p", "fineprint", "Every other browser is signed out. Agent tokens keep working unless you revoke them."),
    );
    const off = el("details", "signin-section");
    off.append(
      el("summary", "", "Turn sign-in off"),
      el("p", "fineprint", "Anyone who can reach the workspace could use it again. Tokens are kept for if you turn it back on."),
      form(
        [field("Current password", "current", "current-password")],
        "Turn sign-in off",
        async (v) => {
          await api("/api/auth/password", "DELETE", { current: v.current });
          render();
        },
        "danger",
      ),
    );
    body.append(change, off);
  }
})();
