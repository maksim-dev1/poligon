// Shared helpers for every poligon page: identity of the session (csrf),
// escaping, toasts and the two dialogs that replace the browser's native
// confirm()/prompt()/alert() — those block the whole tab (and, inside the
// screen wall's iframes, the wall itself) and cannot be styled or read by the
// user next to the thing they are about to destroy.

const csrf = () => (document.cookie.match(/(?:^|;\s*)poligon_csrf=([^;]+)/) || [])[1] || "";
const HW = () => ({ "X-Poligon-CSRF": csrf() });
const HJ = () => ({ "X-Poligon-CSRF": csrf(), "Content-Type": "application/json" });

function esc(s) {
  return String(s ?? "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;" }[c]));
}

function toastHost() {
  let host = document.getElementById("toasts");
  if (!host) {
    host = document.createElement("div");
    host.id = "toasts";
    host.className = "toasts";
    document.body.appendChild(host);
  }
  return host;
}

function toast(msg, kind) {
  const el = document.createElement("div");
  el.className = "toast" + (kind ? " toast--" + kind : "");
  el.textContent = msg;
  toastHost().appendChild(el);
  setTimeout(() => {
    el.style.transition = "opacity .2s";
    el.style.opacity = "0";
    setTimeout(() => el.remove(), 220);
  }, kind === "err" ? 6000 : 3500);
}

// ---- dialogs ---------------------------------------------------------
// Both resolve on close: confirm → bool, prompt → string | null. Escape and a
// click on the backdrop always mean "cancel", never "do it".

function openDialog(innerHTML, wire) {
  return new Promise(resolve => {
    const back = document.createElement("div");
    back.className = "modal-backdrop";
    back.innerHTML = `<form class="modal" onsubmit="return false">${innerHTML}</form>`;
    document.body.appendChild(back);

    let done = false;
    const close = value => {
      if (done) return;
      done = true;
      document.removeEventListener("keydown", onKey);
      back.remove();
      resolve(value);
    };
    const onKey = e => { if (e.key === "Escape") close(null); };
    document.addEventListener("keydown", onKey);
    back.addEventListener("mousedown", e => { if (e.target === back) close(null); });
    wire(back.querySelector(".modal"), close);
  });
}

function dialogActions(cancelText, okText, danger) {
  return `<div class="modal-actions">
    <button class="btn" type="button" data-act="cancel">${esc(cancelText)}</button>
    <button class="btn ${danger ? "btn--danger" : "btn--primary"}" type="submit" data-act="ok">${esc(okText)}</button>
  </div>`;
}

// confirmDialog({ title, body, okText, danger }) → Promise<boolean>
function confirmDialog(opts) {
  const o = typeof opts === "string" ? { title: opts } : (opts || {});
  const html = `<h2>${esc(o.title || "Подтвердите")}</h2>
    ${o.body ? `<p class="modal-body">${esc(o.body)}</p>` : ""}
    ${dialogActions(o.cancelText || "Отмена", o.okText || "Продолжить", o.danger !== false)}`;
  return openDialog(html, (form, close) => {
    form.querySelector('[data-act="cancel"]').onclick = () => close(false);
    form.onsubmit = () => close(true);
    form.querySelector('[data-act="ok"]').focus();
  }).then(v => v === true);
}

// promptDialog({ title, label, value, placeholder, hint, multiline, okText })
//   → Promise<string | null>
function promptDialog(opts) {
  const o = opts || {};
  const field = o.multiline
    ? `<textarea class="input input--area" id="dlg-in" rows="${o.rows || 4}" placeholder="${esc(o.placeholder || "")}"></textarea>`
    : `<input class="input" id="dlg-in" type="text" placeholder="${esc(o.placeholder || "")}">`;
  const html = `<h2>${esc(o.title || "")}</h2>
    ${o.hint ? `<p class="modal-body">${esc(o.hint)}</p>` : ""}
    <label class="field">${o.label ? `<span class="field-label">${esc(o.label)}</span>` : ""}${field}</label>
    ${dialogActions(o.cancelText || "Отмена", o.okText || "ОК", false)}`;
  return openDialog(html, (form, close) => {
    const input = form.querySelector("#dlg-in");
    input.value = o.value || "";
    form.querySelector('[data-act="cancel"]').onclick = () => close(null);
    form.onsubmit = () => close(input.value.trim() ? input.value : null);
    // a textarea keeps Enter for newlines; Cmd/Ctrl+Enter submits
    if (o.multiline) {
      input.addEventListener("keydown", e => {
        if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); form.requestSubmit(); }
      });
    }
    input.focus();
    input.select();
  });
}
