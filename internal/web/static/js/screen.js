// Screen tab: noVNC in the page, talking raw RFB over the server's
// /vnc WebSocket, which relays to the sandbox display (Android sidecar,
// KubeVirt VNC, Mac agent).
import RFB from '/vendor/novnc/core/rfb.js';

// X keysyms used for the shortcuts.
const XK_ALT_L = 0xffe9;
const XK_CONTROL_L = 0xffe3;

// openScreen mounts a display in el. getPassword resolves the VNC password
// when the server asks for one; onClipboard receives text copied on the
// remote side. It returns controls for the tab.
// opts.android sends scrcpy's paste shortcut instead of Ctrl+V.
export function openScreen(el, name, onStatus, getPassword, onClipboard, opts = {}) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = `${proto}//${location.host}/api/v1/sandboxes/${encodeURIComponent(name)}/vnc`;
  onStatus('Connecting...');
  const rfb = new RFB(el, url, { wsProtocols: ['binary'] });
  rfb.scaleViewport = true;
  rfb.resizeSession = false;
  rfb.background = '#0c0a09';
  let closed = false;
  rfb.addEventListener('connect', () => { onStatus('Connected'); rfb.focus(); });
  rfb.addEventListener('disconnect', (e) => {
    if (closed) return;
    onStatus(e.detail.clean
      ? 'Disconnected'
      : 'Could not reach the screen. It may still be starting; reconnect in a moment.');
  });
  rfb.addEventListener('credentialsrequired', async () => {
    try {
      rfb.sendCredentials({ password: await getPassword() });
    } catch {
      onStatus('This screen needs a password and none is known.');
    }
  });
  rfb.addEventListener('clipboard', (e) => onClipboard && onClipboard(e.detail.text));

  // Ctrl+V: noVNC would forward the keystroke before the browser exposes
  // the clipboard, so take it first: let the paste event fire, then hand
  // the text to the remote clipboard. On Android the screen sidecar types
  // it into the focused field; elsewhere we follow with Ctrl+V.
  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'v') {
      e.stopImmediatePropagation(); // keep it from noVNC, but allow the paste
    }
  };
  const onPaste = (e) => {
    // Pastes into form fields (the clipboard box) are theirs.
    if (e.target instanceof Element && e.target.closest('textarea, input')) return;
    const text = e.clipboardData && e.clipboardData.getData('text/plain');
    if (!text) return;
    e.preventDefault();
    paste(text);
  };
  const paste = (text) => {
    // noVNC prefers the extended clipboard, where the server fetches the
    // text lazily; scrcpy reads the X clipboard right away on Ctrl+V and
    // gets nothing. Plain (Latin-1) cut text is set at once, so use it when
    // the text fits. This touches a private noVNC field; the vendored
    // version is pinned (see vendor/novnc/README.md).
    const latin1 = [...text].every((ch) => ch.codePointAt(0) <= 0xff);
    const caps = rfb._clipboardServerCapabilitiesFormats;
    if (latin1 && caps) rfb._clipboardServerCapabilitiesFormats = {};
    try {
      rfb.clipboardPasteFrom(text);
    } finally {
      if (latin1 && caps) rfb._clipboardServerCapabilitiesFormats = caps;
    }
    if (opts.android) {
      rfb.focus();
      return;
    }
    // Give the server a moment to set its clipboard before the keystroke.
    setTimeout(() => {
      rfb.sendKey(XK_CONTROL_L, 'ControlLeft', true);
      rfb.sendKey(0x76, 'KeyV');
      rfb.sendKey(XK_CONTROL_L, 'ControlLeft', false);
      rfb.focus();
    }, 150);
  };
  el.addEventListener('keydown', onKey, true);
  document.addEventListener('paste', onPaste);

  rfb.addEventListener('securityfailure', (e) => onStatus(`Screen refused the connection: ${e.detail.reason || 'security failure'}`));
  return {
    close() {
      closed = true;
      el.removeEventListener('keydown', onKey, true);
      document.removeEventListener('paste', onPaste);
      try { rfb.disconnect(); } catch { /* already closed */ }
    },
    // paste sends text to the remote clipboard and pastes it.
    paste,
    // altKey sends Alt+<letter>, scrcpy's shortcut modifier.
    altKey(ch) {
      const sym = ch.charCodeAt(0);
      rfb.sendKey(XK_ALT_L, 'AltLeft', true);
      rfb.sendKey(sym, `Key${ch.toUpperCase()}`);
      rfb.sendKey(XK_ALT_L, 'AltLeft', false);
      rfb.focus();
    },
    ctrlAltDel() { rfb.sendCtrlAltDel(); },
  };
}
