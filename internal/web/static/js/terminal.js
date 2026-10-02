// Browser terminal: xterm.js on one side, the server WebSocket on the other.
// See internal/api/terminal.go for the protocol.
import { Terminal } from '/vendor/xterm/xterm.mjs';
import { FitAddon } from '/vendor/xterm/addon-fit.mjs';

const theme = {
  background: '#0c0a09',
  foreground: '#e7e5e4',
  cursor: '#fb923c',
  selectionBackground: '#44403c',
};

// openTerminal mounts a terminal in el and connects it. onStatus receives
// connection state text. It returns a function that tears everything down.
export function openTerminal(el, name, onStatus) {
  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, "Cascadia Mono", "JetBrains Mono", Menlo, Consolas, monospace',
    fontSize: 13,
    scrollback: 5000,
    theme,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/api/v1/sandboxes/${encodeURIComponent(name)}/terminal`);
  ws.binaryType = 'arraybuffer';
  const enc = new TextEncoder();
  let closed = false;

  const sendSize = () => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    }
  };

  onStatus('Connecting...');
  ws.onopen = () => {
    onStatus('Connected');
    sendSize();
    term.focus();
  };
  ws.onmessage = (e) => {
    if (typeof e.data === 'string') {
      term.write(`\r\n\x1b[2m[${e.data}]\x1b[0m\r\n`);
      return;
    }
    term.write(new Uint8Array(e.data));
  };
  ws.onclose = (e) => {
    if (!closed) onStatus(e.code === 1000 ? 'Session ended' : 'Disconnected');
  };
  ws.onerror = () => onStatus('Could not connect. Is the sandbox running?');

  const sub = term.onData((d) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
  });
  const ro = new ResizeObserver(() => {
    try { fit.fit(); } catch { /* element hidden */ }
  });
  ro.observe(el);
  const rs = term.onResize(sendSize);

  return () => {
    closed = true;
    ro.disconnect();
    sub.dispose();
    rs.dispose();
    ws.close();
    term.dispose();
  };
}
