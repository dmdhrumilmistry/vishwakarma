package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dmdhrumilmistry/vishwakarma/internal/config"
	"github.com/dmdhrumilmistry/vishwakarma/internal/macos"
	"github.com/dmdhrumilmistry/vishwakarma/internal/sandbox"
)

// vncUpgrader serves noVNC, which may ask for the "binary" subprotocol.
var vncUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
	CheckOrigin:     sameOrigin,
	Subprotocols:    []string{"binary"},
}

// vnc relays raw RFB between the console's noVNC client and the sandbox
// display: a container's VNC port (Android screen sidecar, Docker-OSX), the
// KubeVirt VNC subresource, or a Mac agent's VNC relay.
func (s *Server) vnc(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	sb, err := s.mgr.Get(r.Context(), r.PathValue("name"), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !sb.Screen {
		writeError(w, http.StatusBadRequest, "this sandbox has no screen")
		return
	}
	if sb.Status != sandbox.StatusRunning {
		writeError(w, http.StatusConflict, "the sandbox is "+strings.ToLower(sb.Status)+"; start it first")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Open the far side first, so a failure is a plain HTTP error.
	var tcp net.Conn
	var far *websocket.Conn
	switch sb.Kind {
	case config.KindContainer:
		addr, err := s.mgr.ScreenAddr(r.Context(), sb)
		if err != nil {
			s.fail(w, err)
			return
		}
		tcp, err = net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, "the screen is not up yet: "+err.Error())
			return
		}
	case config.KindVM:
		far, err = s.clients.VMVNC(ctx, s.mgr.Namespace(), sb.Name)
	case config.KindMacOS:
		if pool := s.mgr.MacOS(); pool != nil {
			far, err = pool.VNC(ctx, sb.Host, sb.Name)
		} else {
			err = sandbox.ErrMacOSUnavailable
		}
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if tcp != nil {
		defer tcp.Close()
	}
	if far != nil {
		defer far.Close()
	}

	ws, err := vncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	s.log.Info("screen opened", "sandbox", sb.Name, "kind", sb.Kind, "user", c.Name)
	if tcp != nil {
		macos.BridgeTCP(ws, tcp)
	} else {
		macos.BridgeWS(ws, far)
	}
	s.log.Info("screen closed", "sandbox", sb.Name, "user", c.Name)
}
