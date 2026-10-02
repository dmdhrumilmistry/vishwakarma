// Small DOM and API helpers. Untrusted text only ever goes in via
// textContent (h() children that are strings); never innerHTML.

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, String(v));
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// api calls the REST API. Every request carries the CSRF header the server
// requires for state changes.
export async function api(method, path, body) {
  const opts = {
    method,
    credentials: 'same-origin',
    headers: { 'X-Vishwakarma-Request': '1' },
  };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch('/api/v1' + path, opts);
  if (res.status === 204) return null;
  const type = res.headers.get('Content-Type') || '';
  const data = type.includes('application/json') ? await res.json() : await res.text();
  if (!res.ok) {
    const msg = (data && data.error) || (typeof data === 'string' && data) || res.statusText;
    throw new ApiError(res.status, msg);
  }
  return data;
}

export function toast(msg, bad) {
  const box = document.getElementById('toasts');
  const t = h('div', { class: bad ? 'toast toast-bad' : 'toast', role: bad ? 'alert' : 'status' }, msg);
  box.appendChild(t);
  setTimeout(() => t.remove(), bad ? 7000 : 3500);
}

// confirmDialog resolves true when the user confirms.
export function confirmDialog(title, text, action) {
  return new Promise((resolve) => {
    const done = (v) => { back.remove(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') done(false); };
    const ok = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => done(true) }, action);
    const back = h('div', { class: 'modal-back', onclick: (e) => { if (e.target === back) done(false); } },
      h('div', { class: 'panel modal', role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
        h('h2', {}, title),
        h('p', { class: 'muted' }, text),
        h('div', { class: 'form-actions' },
          h('button', { class: 'btn', type: 'button', onclick: () => done(false) }, 'Cancel'),
          ok)));
    document.body.appendChild(back);
    document.addEventListener('keydown', onKey);
    ok.focus();
  });
}

export function chip(status) {
  const cls = {
    Running: 'chip chip-running',
    Pending: 'chip chip-pending',
    Failed: 'chip chip-failed',
    Terminating: 'chip chip-terminating',
  }[status] || 'chip';
  return h('span', { class: cls }, status);
}

// until formats the time left before t, e.g. "3h 12m".
export function until(t) {
  const ms = new Date(t).getTime() - Date.now();
  if (!isFinite(ms)) return '-';
  if (ms <= 0) return 'expiring';
  const m = Math.floor(ms / 60000);
  const d = Math.floor(m / 1440), hr = Math.floor((m % 1440) / 60), min = m % 60;
  if (d > 0) return `${d}d ${hr}h`;
  if (hr > 0) return `${hr}h ${min}m`;
  return `${min}m`;
}

export function ago(t) {
  const s = Math.floor((Date.now() - new Date(t).getTime()) / 1000);
  if (!isFinite(s)) return '-';
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function fmtDuration(goDuration) {
  // Go durations arrive as "4h0m0s"; show "4h".
  return String(goDuration).replace(/(\d)h0m0s$/, '$1h').replace(/(\d)m0s$/, '$1m');
}

// parseDuration reads Go style "72h0m0s" or "30m" into milliseconds.
export function parseDuration(s) {
  let ms = 0;
  for (const [, n, u] of String(s).matchAll(/(\d+(?:\.\d+)?)(h|m|s)/g)) {
    ms += parseFloat(n) * { h: 3600e3, m: 60e3, s: 1e3 }[u];
  }
  return ms;
}

export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Copied');
  } catch {
    toast('Copy failed; select the text and copy it manually', true);
  }
}

// splitArgs splits a command line on spaces, honoring single and double quotes.
export function splitArgs(s) {
  const out = [];
  let cur = '', quote = null, any = false;
  for (const ch of s) {
    if (quote) {
      if (ch === quote) quote = null; else cur += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch; any = true;
    } else if (/\s/.test(ch)) {
      if (cur || any) { out.push(cur); cur = ''; any = false; }
    } else {
      cur += ch;
    }
  }
  if (cur || any) out.push(cur);
  return out;
}
