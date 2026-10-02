package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/kube"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
)

// Terminal protocol between the console and the server, over one WebSocket:
//   - binary frames carry terminal bytes in both directions
//   - text frames from the browser carry JSON control messages:
//     {"type":"resize","cols":120,"rows":40}
//   - a text frame from the server is a status line shown to the user
//
// Containers get an exec session; VMs get their serial console.

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	CheckOrigin:     sameOrigin,
}

// sameOrigin blocks cross-site WebSocket hijacking: browsers attach cookies
// to WebSocket handshakes from any site, so the Origin must match.
// Non-browser clients send no Origin and authenticate with a bearer token.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return bearer(r)
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

type control struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// defaultShell prefers bash and falls back to sh, with a sane TERM.
const defaultShell = `export TERM=xterm-256color; if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi`

func (s *Server) terminal(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	sb, err := s.mgr.Get(r.Context(), r.PathValue("name"), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	if sb.Status != sandbox.StatusRunning {
		writeError(w, http.StatusConflict, "the sandbox is "+strings.ToLower(sb.Status)+"; start it first")
		return
	}
	var pod string
	if sb.Kind == config.KindContainer {
		p, err := s.mgr.RunningPod(r.Context(), sb)
		if err != nil {
			s.fail(w, err)
			return
		}
		pod = p.Name
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader already replied
	}
	defer ws.Close()
	s.log.Info("terminal opened", "sandbox", sb.Name, "kind", sb.Kind, "user", c.Name)

	// The session ends when the browser goes away or the remote side does.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &wsWriter{ws: ws}
	// Keep idle sessions alive through proxies that drop quiet connections.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					cancel()
					return
				}
			}
		}
	}()

	if sb.Kind == config.KindVM {
		s.vmConsole(ctx, cancel, ws, out, sb.Name)
	} else {
		s.execShell(ctx, cancel, ws, out, pod, s.shellFor(sb))
	}
	s.log.Info("terminal closed", "sandbox", sb.Name, "user", c.Name)
}

func (s *Server) shellFor(sb *sandbox.Sandbox) []string {
	if t, ok := s.mgr.Policy().Template(sb.Template); ok && t.Shell != "" && sb.Image == t.Image {
		return []string{"/bin/sh", "-c", "export TERM=xterm-256color; exec " + t.Shell}
	}
	return []string{"/bin/sh", "-c", defaultShell}
}

func (s *Server) execShell(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn, out *wsWriter, pod string, cmd []string) {
	stdinR, stdinW := io.Pipe()
	sizes := make(chan kube.TermSize, 4)

	go func() {
		defer cancel()
		defer stdinW.Close()
		defer close(sizes)
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				var m control
				if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
					select {
					case sizes <- kube.TermSize{Width: m.Cols, Height: m.Rows}:
					default: // drop bursts; the next resize wins
					}
				}
				continue
			}
			if _, err := stdinW.Write(data); err != nil {
				return
			}
		}
	}()

	err := s.clients.Exec(ctx, s.mgr.Namespace(), pod, sandbox.ContainerName, cmd, stdinR, out, sizes)
	if err != nil && ctx.Err() == nil {
		out.status("session ended: " + err.Error())
	} else if ctx.Err() == nil {
		out.status("session ended")
	}
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

func (s *Server) vmConsole(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn, out *wsWriter, name string) {
	vc, err := s.clients.VMConsole(ctx, s.mgr.Namespace(), name)
	if err != nil {
		out.status("could not open the VM console: " + err.Error())
		return
	}
	defer vc.Close()
	out.status("connected to the serial console; press Enter if the prompt does not show")

	go func() {
		defer cancel()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				continue // resize does not apply to a serial line
			}
			if err := vc.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		vc.Close()
	}()
	for {
		_, data, err := vc.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				out.status("console closed")
			}
			return
		}
		if _, err := out.Write(data); err != nil {
			return
		}
	}
}

// wsWriter serialises writes to the browser socket.
type wsWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsWriter) status(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.ws.WriteMessage(websocket.TextMessage, []byte(msg))
}
