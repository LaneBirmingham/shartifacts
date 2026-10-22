// "Connect an agent", shared by the apps hub and the artifacts workspace: this server's MCP
// address and the commands that connect common agents to it. With sign-in on (signin.js),
// it first makes the agent a token and puts it in the commands. Any [data-connect] element
// opens it. Also window.shartifactsCopy, a clipboard copy that works over plain-HTTP LAN
// addresses too, where browsers withhold navigator.clipboard.
(() => {
  async function copy(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch {}
    const t = document.createElement("textarea");
    t.value = text;
    t.setAttribute("readonly", "");
    t.style.cssText = "position:fixed;top:0;left:0;opacity:0";
    (document.querySelector("dialog[open]") || document.body).append(t);
    t.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {}
    t.remove();
    return ok;
  }
  window.shartifactsCopy = copy;

  const url = `${location.origin}/mcp`;
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
  // Each client's command, with the token in it when sign-in is on (token is "" when off).
  const clients = (token) => [
    ["Claude Code", `claude mcp add --transport http --scope user shartifacts ${url}` + (token ? ` --header "Authorization: Bearer ${token}"` : "")],
    [
      "Codex",
      token
        ? `# Put the export in your shell profile too, so Codex always has it.\nexport SHARTIFACTS_TOKEN=${token}\ncodex mcp add shartifacts --url ${url} --bearer-token-env-var SHARTIFACTS_TOKEN`
        : `codex mcp add shartifacts --url ${url}`,
    ],
    [
      "Cursor, Claude Code's .mcp.json and other mcpServers files",
      JSON.stringify({ mcpServers: { shartifacts: token ? { type: "http", url, headers: { Authorization: `Bearer ${token}` } } : { type: "http", url } } }, null, 2),
    ],
  ];

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  };
  const copyButton = (text, label = "Copy") => {
    const b = el("button", "quiet", label);
    b.type = "button";
    b.onclick = async () => {
      b.textContent = (await copy(text)) ? "Copied" : "Select and copy";
      setTimeout(() => (b.textContent = label), 1600);
    };
    return b;
  };

  const dialog = el("dialog", "connect-dialog");
  dialog.id = "connect-dialog";
  dialog.setAttribute("aria-labelledby", "connect-title");
  const head = el("div", "dialog-heading");
  const close = el("button", "", "×");
  close.type = "button";
  close.dataset.close = "";
  close.setAttribute("aria-label", "Close");
  close.onclick = () => dialog.close();
  head.append(el("span", "eyebrow", "CONNECT AN AGENT"), close);
  const title = el("h2", "", "Give your agent a place to make things.");
  title.id = "connect-title";
  const intro = el("p", "", "Agents connect over MCP. Once connected, yours can make artifacts and apps here, change them when you ask, and hand you links to open on any of your devices.");

  const endpoint = el("label", "", "MCP endpoint (Streamable HTTP)");
  const row = el("div", "copy-row");
  const input = el("input");
  input.readOnly = true;
  input.value = url;
  row.append(input, copyButton(url));
  endpoint.append(row);

  const list = el("div", "connect-clients");
  function showClients(token) {
    list.replaceChildren();
    for (const [name, snippet] of clients(token)) {
      const item = el("div", "connect-client");
      const top = el("div", "connect-client-head");
      top.append(el("strong", "", name), copyButton(snippet));
      item.append(top, el("pre", "", snippet));
      list.append(item);
    }
  }

  // With sign-in on: name a token, make it, and show the commands with it. It is shown
  // only in this dialog, once.
  const tokenStep = el("form", "connect-token");
  tokenStep.hidden = true;
  const tokenName = el("input");
  tokenName.name = "name";
  tokenName.maxLength = 60;
  tokenName.required = true;
  tokenName.value = "Claude Code";
  tokenName.setAttribute("aria-label", "Token name");
  const makeToken = el("button", "primary", "Make a token");
  makeToken.type = "submit";
  const tokenNote = el("p", "fineprint");
  const tokenError = el("p", "form-error");
  const tokenRow = el("div", "copy-row");
  tokenRow.append(tokenName, makeToken);
  tokenStep.append(el("strong", "", "Sign-in is on, so each agent needs a token."), tokenNote, tokenRow, tokenError);
  tokenStep.addEventListener("submit", async (e) => {
    e.preventDefault();
    tokenError.textContent = "";
    makeToken.disabled = true;
    try {
      const r = await fetch("/api/auth/tokens", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: tokenName.value }) });
      const t = await r.json();
      if (!r.ok) throw new Error(t.error || `Request failed (${r.status})`);
      tokenNote.textContent = `Made “${t.name}”. The commands below include it: copy one now, because the token isn't shown again. Revoke it any time under Sign-in.`;
      tokenRow.hidden = true;
      showClients(t.token);
    } catch (err) {
      tokenError.textContent = err.message;
    } finally {
      makeToken.disabled = false;
    }
  });

  const next = el("p", "connect-next");
  next.append("Then ask it something like ", el("em", "", "“make me a packing checklist in shartifacts”"), " or ", el("em", "", "“build a small expenses app in shartifacts”"), ".");

  const note = el("p", "fineprint");
  const more = el("a", "", "Other clients");
  more.href = "https://github.com/LaneBirmingham/shartifacts/blob/main/docs/agents.md";
  more.target = "_blank";
  more.rel = "noopener";

  // Fill the dialog in for sign-in being on or off, as it is when it opens.
  async function prepare() {
    const auth = window.shartifactsAuth ? await window.shartifactsAuth.refresh() : { enabled: false };
    const on = !!auth.enabled;
    tokenStep.hidden = !on;
    tokenRow.hidden = false;
    tokenError.textContent = "";
    tokenNote.textContent = "Name it for the agent and device, so you can tell tokens apart and revoke one on its own.";
    showClients(on ? "<token>" : "");
    note.replaceChildren(
      local
        ? "This address works only on this computer. For agents on other devices, open shartifacts by this computer's LAN or Tailscale address (with Docker, set SHARTIFACTS_BIND in .env first) and connect them to that. "
        : on
          ? "Only agents with a token get in. "
          : "Anyone who can reach this address can read and change everything here and deploy apps, so keep it on your own network or tailnet, or set a password under Sign-in. ",
      more,
      ".",
    );
  }

  dialog.append(head, title, intro, endpoint, tokenStep, list, next, note);
  document.body.append(dialog);
  dialog.addEventListener("click", (e) => {
    if (e.target === dialog) dialog.close();
  });
  document.addEventListener("click", async (e) => {
    if (!e.target.closest?.("[data-connect]")) return;
    await prepare();
    dialog.showModal();
  });
})();
