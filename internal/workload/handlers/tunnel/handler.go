package tunnel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	workloadcheckpoint "github.com/Beam-Network/beam/internal/workload/checkpoint"
	"github.com/Beam-Network/beam/internal/workload/contracts"
	"github.com/Beam-Network/beam/internal/workload/domain"
	workloadprogress "github.com/Beam-Network/beam/internal/workload/progress"
)

const tunnelCheckpointSchema = "beam.tunnel.session/1"

type tunnelCheckpoint struct {
	TunnelID      string `json:"tunnel_id"`
	Protocol      string `json:"protocol"`
	ListenAddress string `json:"listen_address"`
	Phase         string `json:"phase"`
	Connections   int64  `json:"connections,omitempty"`
	Requests      int64  `json:"requests,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
}

type Config struct {
	AllowPublicListeners bool
	MaxConnections       int
	DialTimeout          time.Duration
}

type Activation struct {
	Address  string
	Protocol string
}

type Handler struct {
	kind   domain.Kind
	config Config
	mu     sync.RWMutex
	active map[string]Activation
}

func NewHTTPHandler(config Config) *Handler { return newHandler(domain.KindTunnelHTTP, config) }
func NewTCPHandler(config Config) *Handler  { return newHandler(domain.KindTunnelTCP, config) }

func newHandler(kind domain.Kind, config Config) *Handler {
	if config.MaxConnections <= 0 {
		config.MaxConnections = 256
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	return &Handler{kind: kind, config: config, active: make(map[string]Activation)}
}

func (h *Handler) Kind() domain.Kind { return h.kind }

func (h *Handler) Activation(workloadKey string) (Activation, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	activation, ok := h.active[workloadKey]
	return activation, ok
}

func (h *Handler) Validate(spec domain.Spec) error {
	assignment, err := decodeAssignment(spec)
	if err != nil {
		return err
	}
	if h.kind == domain.KindTunnelHTTP && assignment.Protocol != "http" {
		return errors.New("HTTP tunnel handler requires protocol http")
	}
	if h.kind == domain.KindTunnelTCP && assignment.Protocol != "tcp" {
		return errors.New("TCP tunnel handler requires protocol tcp")
	}
	if err := validateListener(assignment.ListenAddress, assignment.IngressToken, h.config.AllowPublicListeners); err != nil {
		return err
	}
	if !networkTargetAllowed(spec.Security.NetworkTargets, assignment.Target) {
		return fmt.Errorf("tunnel target %q is not authorized by workload policy", assignment.Target)
	}
	if h.kind == domain.KindTunnelHTTP {
		target, err := url.Parse(assignment.Target)
		if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
			return errors.New("HTTP tunnel target must be an absolute HTTP(S) URL")
		}
	} else if _, _, err := net.SplitHostPort(assignment.Target); err != nil {
		return errors.New("TCP tunnel target must be host:port")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, spec domain.Spec) (domain.Result, error) {
	assignment, err := decodeAssignment(spec)
	if err != nil {
		return domain.Result{}, err
	}
	if err := h.Validate(spec); err != nil {
		return domain.Result{}, err
	}
	if h.kind == domain.KindTunnelHTTP {
		return h.executeHTTP(ctx, spec.Key(), assignment)
	}
	return h.executeTCP(ctx, spec.Key(), assignment)
}

func (h *Handler) executeHTTP(ctx context.Context, key string, assignment contracts.TunnelAssignment) (domain.Result, error) {
	target, _ := url.Parse(assignment.Target)
	baseListener, err := net.Listen("tcp", assignment.ListenAddress)
	if err != nil {
		return domain.Result{}, err
	}
	listener := newLimitedListener(baseListener, h.config.MaxConnections)
	defer listener.Close()
	h.activate(key, listener.Addr().String(), "http")
	workloadprogress.Report(ctx, map[string]string{"listen_address": listener.Addr().String(), "protocol": "http"})
	defer h.deactivate(key)
	var requests atomic.Int64
	var bytesTransferred atomic.Int64
	if err := saveTunnelCheckpoint(ctx, tunnelCheckpoint{TunnelID: assignment.TunnelID, Protocol: "http", ListenAddress: listener.Addr().String(), Phase: "active"}); err != nil {
		return domain.Result{}, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		Proxy:             nil,
		DialContext:       (&net.Dialer{Timeout: h.config.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true,
	}
	proxy.ErrorHandler = func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
		http.Error(response, proxyErr.Error(), http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorized, bearerWasAssignment := authorizeRequest(request.Header.Get("Authorization"), request.Header.Get("X-Beam-Assignment-Token"), assignment.IngressToken)
		if !authorized {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		if bearerWasAssignment {
			request.Header.Del("Authorization")
		}
		request.Header.Del("X-Beam-Assignment-Token")
		requests.Add(1)
		if request.Body != nil {
			request.Body = &countingReadCloser{ReadCloser: request.Body, count: &bytesTransferred}
		}
		proxy.ServeHTTP(&countingResponseWriter{ResponseWriter: response, count: &bytesTransferred}, request)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = server.Shutdown(shutdownContext)
		cancel()
		<-done
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return domain.Result{}, err
		}
	}
	if err := saveTunnelCheckpoint(ctx, tunnelCheckpoint{TunnelID: assignment.TunnelID, Protocol: "http", ListenAddress: listener.Addr().String(), Phase: "stopped", Requests: requests.Load(), Bytes: bytesTransferred.Load()}); err != nil {
		return domain.Result{}, err
	}
	return domain.Result{BytesProcessed: bytesTransferred.Load(), Outputs: map[string]string{
		"listen_address": listener.Addr().String(), "requests": strconv.FormatInt(requests.Load(), 10),
	}}, nil
}

func (h *Handler) executeTCP(ctx context.Context, key string, assignment contracts.TunnelAssignment) (domain.Result, error) {
	listener, err := net.Listen("tcp", assignment.ListenAddress)
	if err != nil {
		return domain.Result{}, err
	}
	defer listener.Close()
	h.activate(key, listener.Addr().String(), "tcp")
	workloadprogress.Report(ctx, map[string]string{"listen_address": listener.Addr().String(), "protocol": "tcp"})
	defer h.deactivate(key)
	if err := saveTunnelCheckpoint(ctx, tunnelCheckpoint{TunnelID: assignment.TunnelID, Protocol: "tcp", ListenAddress: listener.Addr().String(), Phase: "active"}); err != nil {
		return domain.Result{}, err
	}
	semaphore := make(chan struct{}, h.config.MaxConnections)
	var connections sync.Map
	var wait sync.WaitGroup
	var accepted atomic.Int64
	var transferred atomic.Int64
	done := make(chan error, 1)
	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				client.Close()
				continue
			default:
				client.Close()
				continue
			}
			connections.Store(client, struct{}{})
			wait.Add(1)
			go func() {
				defer wait.Done()
				defer func() { <-semaphore }()
				defer connections.Delete(client)
				defer client.Close()
				if assignment.IngressToken != "" {
					_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
					token := make([]byte, len(assignment.IngressToken)+1)
					if _, err := io.ReadFull(client, token); err != nil || token[len(token)-1] != '\n' ||
						subtle.ConstantTimeCompare(token[:len(token)-1], []byte(assignment.IngressToken)) != 1 {
						return
					}
					_ = client.SetReadDeadline(time.Time{})
				}
				accepted.Add(1)
				upstream, err := net.DialTimeout("tcp", assignment.Target, h.config.DialTimeout)
				if err != nil {
					return
				}
				connections.Store(upstream, struct{}{})
				defer connections.Delete(upstream)
				defer upstream.Close()
				copyBoth(client, upstream, &transferred)
			}()
		}
	}()
	select {
	case <-ctx.Done():
		_ = listener.Close()
		connections.Range(func(connection, _ any) bool { _ = connection.(net.Conn).Close(); return true })
		<-done
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return domain.Result{}, err
		}
	}
	wait.Wait()
	if err := saveTunnelCheckpoint(ctx, tunnelCheckpoint{TunnelID: assignment.TunnelID, Protocol: "tcp", ListenAddress: listener.Addr().String(), Phase: "stopped", Connections: accepted.Load(), Bytes: transferred.Load()}); err != nil {
		return domain.Result{}, err
	}
	return domain.Result{BytesProcessed: transferred.Load(), Outputs: map[string]string{
		"listen_address": listener.Addr().String(), "connections": strconv.FormatInt(accepted.Load(), 10),
	}}, nil
}

func saveTunnelCheckpoint(ctx context.Context, value tunnelCheckpoint) error {
	err := workloadcheckpoint.Save(ctx, tunnelCheckpointSchema, map[string]string{
		"tunnel_id": value.TunnelID, "protocol": value.Protocol, "phase": value.Phase,
	}, value)
	if errors.Is(err, workloadcheckpoint.ErrUnavailable) {
		return nil
	}
	return err
}

func (h *Handler) activate(key, address, protocol string) {
	h.mu.Lock()
	h.active[key] = Activation{Address: address, Protocol: protocol}
	h.mu.Unlock()
}

func (h *Handler) deactivate(key string) {
	h.mu.Lock()
	delete(h.active, key)
	h.mu.Unlock()
}

func decodeAssignment(spec domain.Spec) (contracts.TunnelAssignment, error) {
	var assignment contracts.TunnelAssignment
	if err := json.Unmarshal(spec.Payload, &assignment); err != nil {
		return assignment, err
	}
	if assignment.TunnelID == "" || assignment.ListenAddress == "" || assignment.Target == "" {
		return assignment, errors.New("tunnel id, listen address, and target are required")
	}
	return assignment, nil
}

func validateListener(address, token string, allowPublic bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("tunnel listen address must be host:port")
	}
	ip := net.ParseIP(host)
	public := !(host == "localhost" || (ip != nil && ip.IsLoopback()))
	if public && !allowPublic {
		return errors.New("public tunnel listeners are disabled")
	}
	if public && token == "" {
		return errors.New("public tunnel listeners require an ingress token")
	}
	return nil
}

func networkTargetAllowed(allowed []string, target string) bool {
	if len(allowed) == 0 {
		return false
	}
	parsed, _ := url.Parse(target)
	host := target
	if parsed != nil && parsed.Host != "" {
		host = parsed.Host
	}
	hostname := host
	if value, _, err := net.SplitHostPort(host); err == nil {
		hostname = value
	}
	for _, candidate := range allowed {
		if candidate == target || candidate == host || candidate == hostname {
			return true
		}
	}
	return false
}

func authorizeRequest(authorization, assignmentToken, expected string) (bool, bool) {
	if expected == "" {
		return true, false
	}
	if len(assignmentToken) == len(expected) && subtle.ConstantTimeCompare([]byte(assignmentToken), []byte(expected)) == 1 {
		return true, false
	}
	provided := strings.TrimPrefix(authorization, "Bearer ")
	valid := provided != authorization && len(provided) == len(expected) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
	return valid, valid
}

func copyBoth(left, right net.Conn, counter *atomic.Int64) {
	var wait sync.WaitGroup
	wait.Add(2)
	copyDirection := func(destination, source net.Conn) {
		defer wait.Done()
		bytes, _ := io.Copy(destination, source)
		counter.Add(bytes)
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	go copyDirection(left, right)
	go copyDirection(right, left)
	wait.Wait()
}

type countingReadCloser struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *countingReadCloser) Read(value []byte) (int, error) {
	read, err := r.ReadCloser.Read(value)
	r.count.Add(int64(read))
	return read, err
}

type countingResponseWriter struct {
	http.ResponseWriter
	count *atomic.Int64
}

type limitedListener struct {
	net.Listener
	semaphore chan struct{}
}

func newLimitedListener(listener net.Listener, maximum int) *limitedListener {
	return &limitedListener{Listener: listener, semaphore: make(chan struct{}, maximum)}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.semaphore <- struct{}{}
	return &limitedConnection{Conn: connection, release: func() { <-l.semaphore }}, nil
}

type limitedConnection struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *limitedConnection) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

func (w *countingResponseWriter) Write(value []byte) (int, error) {
	written, err := w.ResponseWriter.Write(value)
	w.count.Add(int64(written))
	return written, err
}

func (w *countingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("HTTP connection hijacking is unavailable")
	}
	return hijacker.Hijack()
}

func (w *countingResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}
