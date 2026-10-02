// Package kube builds Kubernetes clients and the streaming connections the
// web terminal needs: pod exec and the KubeVirt serial console.
package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// Clients bundles what the server talks to the cluster with.
type Clients struct {
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
}

// New loads a kubeconfig file when given, else the in-cluster config.
func New(kubeconfig string) (*Clients, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = "vishwakarma"
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Config: cfg, Kube: kc, Dynamic: dc}, nil
}

// KubeVirtServed reports whether the kubevirt.io/v1 API is installed.
func (c *Clients) KubeVirtServed(ctx context.Context) bool {
	_, err := c.Kube.Discovery().ServerResourcesForGroupVersion("kubevirt.io/v1")
	return err == nil
}

// Ready checks the API server is reachable and the sandbox namespace is
// readable with the server's own permissions.
func (c *Clients) Ready(ctx context.Context, namespace string) error {
	_, err := c.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	return err
}

// APIServerAddresses returns the IPs behind the "kubernetes" Service, the
// addresses pods really connect to after Service DNAT.
func (c *Clients) APIServerAddresses(ctx context.Context) ([]string, error) {
	slice, err := c.Kube.DiscoveryV1().EndpointSlices("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ep := range slice.Endpoints {
		out = append(out, ep.Addresses...)
	}
	return out, nil
}

// TermSize is a terminal size change.
type TermSize = remotecommand.TerminalSize

// Exec runs cmd in a pod container with a TTY, wiring stdin and stdout
// until the command exits or ctx is cancelled.
func (c *Clients) Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer, sizes <-chan TermSize) error {
	req := c.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	ws, err := remotecommand.NewWebSocketExecutor(c.Config, "GET", req.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(c.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		// API servers older than 1.30 do not speak the websocket protocol.
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Tty:               true,
		TerminalSizeQueue: sizeQueue(sizes),
	})
}

type sizeQueue <-chan TermSize

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q
	if !ok {
		return nil
	}
	return &s
}

// VMConsole opens the serial console of a running VirtualMachineInstance.
// Frames are raw bytes in both directions.
func (c *Clients) VMConsole(ctx context.Context, ns, name string) (*websocket.Conn, error) {
	return c.vmiStream(ctx, ns, name, "console")
}

// VMVNC opens the VNC display of a running VirtualMachineInstance: raw RFB
// in binary frames.
func (c *Clients) VMVNC(ctx context.Context, ns, name string) (*websocket.Conn, error) {
	return c.vmiStream(ctx, ns, name, "vnc")
}

func (c *Clients) vmiStream(ctx context.Context, ns, name, sub string) (*websocket.Conn, error) {
	host := strings.TrimSuffix(c.Config.Host, "/")
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https", "":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/%s", ns, name, sub)

	tlsCfg, err := rest.TLSConfigFor(c.Config)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	token := c.Config.BearerToken
	if c.Config.BearerTokenFile != "" {
		// Re-read on every connection: projected tokens rotate.
		if b, err := os.ReadFile(c.Config.BearerTokenFile); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	} else if c.Config.Username != "" {
		req := &http.Request{Header: header}
		req.SetBasicAuth(c.Config.Username, c.Config.Password)
	}
	d := websocket.Dialer{
		TLSClientConfig: tlsCfg,
		Subprotocols:    []string{"plain.kubevirt.io"},
		Proxy:           http.ProxyFromEnvironment,
	}
	conn, resp, err := d.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("%s: %s: %s", sub, resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("%s: %w", sub, err)
	}
	return conn, nil
}
