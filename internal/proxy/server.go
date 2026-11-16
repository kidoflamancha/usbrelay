package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoDevice = errors.New("proxy: no ready mobile device connected")

// Stats tracks active connections and forwarded byte counters for a device session.
type Stats struct {
	TCP       atomic.Int64
	BytesUp   atomic.Uint64 // PC -> Phone -> Internet
	BytesDown atomic.Uint64 // Internet -> Phone -> PC
}

func (s *Stats) Snapshot() (tcp int64, up, down uint64) {
	if s == nil {
		return 0, 0, 0
	}
	return s.TCP.Load(), s.BytesUp.Load(), s.BytesDown.Load()
}

// TargetDialer opens an outbound stream to host:port through the connected phone.
type TargetDialer interface {
	DialTarget(ctx context.Context, host string, port uint16) (io.ReadWriteCloser, error)
}

type backend struct {
	dialer TargetDialer
	stats  *Stats
}

// Server exposes a local mixed SOCKS5 + HTTP/HTTPS proxy on the desktop and forwards
// connections to the currently active mobile device backend.
type Server struct {
	addr string
	log  *slog.Logger

	mu      sync.RWMutex
	active  *backend
	ln      net.Listener
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewServer(addr string, log *slog.Logger) *Server {
	if addr == "" {
		addr = "127.0.0.1:1080"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{addr: addr, log: log}
}

func (s *Server) SetBackend(d TargetDialer, st *Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == nil {
		s.active = nil
		return
	}
	s.active = &backend{dialer: d, stats: st}
}

func (s *Server) currentBackend() (*backend, error) {
	s.mu.RLock()
	b := s.active
	s.mu.RUnlock()
	if b == nil || b.dialer == nil {
		return nil, ErrNoDevice
	}
	return b, nil
}

// ListenAndServe starts accepting proxy connections until ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()

	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.serveConn(s.ctx, c)
		}(conn)
	}
	s.wg.Wait()
	return nil
}

// Addr returns the bound listener address (useful when listening on :0 in tests).
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(c, 32*1024)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x05 {
		s.handleSOCKS5(ctx, c, br)
		return
	}
	s.handleHTTP(ctx, c, br)
}

func (s *Server) handleSOCKS5(ctx context.Context, c net.Conn, br *bufio.Reader) {
	var verN [2]byte
	if _, err := io.ReadFull(br, verN[:]); err != nil {
		return
	}
	if verN[0] != 0x05 {
		return
	}
	methods := make([]byte, verN[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	hasNoAuth := false
	for _, m := range methods {
		if m == 0x00 {
			hasNoAuth = true
			break
		}
	}
	if !hasNoAuth {
		_, _ = c.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	var reqHdr [4]byte
	if _, err := io.ReadFull(br, reqHdr[:]); err != nil {
		return
	}
	if reqHdr[0] != 0x05 || reqHdr[1] != 0x01 {
		// Only CMD=CONNECT (0x01) is supported.
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	var host string
	switch reqHdr[3] {
	case 0x01: // IPv4
		var ip [4]byte
		if _, err := io.ReadFull(br, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	case 0x04: // IPv6
		var ip [16]byte
		if _, err := io.ReadFull(br, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	case 0x03: // Domain
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return
		}
		dom := make([]byte, l[0])
		if _, err := io.ReadFull(br, dom); err != nil {
			return
		}
		host = string(dom)
	default:
		_, _ = c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(br, portBuf[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf[:])

	b, err := s.currentBackend()
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	remote, err := b.dialer.DialTarget(dctx, host, port)
	cancel()
	if err != nil {
		s.log.Debug("socks5 dial failed", "host", host, "port", port, "err", err)
		_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = remote.Close()
		return
	}

	s.log.Debug("socks5 open", "target", net.JoinHostPort(host, strconv.Itoa(int(port))))
	relay(ctx, &bufReadWriteCloser{Conn: c, br: br}, remote, b.stats)
	s.log.Debug("socks5 close", "target", net.JoinHostPort(host, strconv.Itoa(int(port))))
}

func (s *Server) handleHTTP(ctx context.Context, c net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	b, err := s.currentBackend()
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	if req.Method == http.MethodConnect {
		host, port, err := parseHostPort(req.Host, 443)
		if err != nil {
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
			return
		}
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		remote, err := b.dialer.DialTarget(dctx, host, port)
		cancel()
		if err != nil {
			s.log.Debug("http connect failed", "host", host, "port", port, "err", err)
			_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			return
		}
		if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			_ = remote.Close()
			return
		}
		s.log.Debug("http connect open", "target", net.JoinHostPort(host, strconv.Itoa(int(port))))
		relay(ctx, &bufReadWriteCloser{Conn: c, br: br}, remote, b.stats)
		s.log.Debug("http connect close", "target", net.JoinHostPort(host, strconv.Itoa(int(port))))
		return
	}

	targetAddr := req.URL.Host
	if targetAddr == "" {
		targetAddr = req.Host
	}
	host, port, err := parseHostPort(targetAddr, 80)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	remote, err := b.dialer.DialTarget(dctx, host, port)
	cancel()
	if err != nil {
		s.log.Debug("http dial failed", "host", host, "port", port, "err", err)
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	// Rewrite proxy-form request into origin-form before forwarding to remote server.
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("Connection", "close")

	if err := req.Write(remote); err != nil {
		_ = remote.Close()
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	s.log.Debug("http forward", "target", net.JoinHostPort(host, strconv.Itoa(int(port))))
	relay(ctx, &bufReadWriteCloser{Conn: c, br: br}, remote, b.stats)
}

func parseHostPort(raw string, defaultPort uint16) (string, uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, errors.New("empty host")
	}
	if strings.Contains(raw, ":") && (!strings.HasPrefix(raw, "[") || strings.Contains(raw, "]:")) {
		h, pStr, err := net.SplitHostPort(raw)
		if err != nil {
			return "", 0, err
		}
		p, err := strconv.Atoi(pStr)
		if err != nil || p <= 0 || p > 65535 {
			return "", 0, fmt.Errorf("invalid port %q", pStr)
		}
		return h, uint16(p), nil
	}
	return strings.Trim(raw, "[]"), defaultPort, nil
}

func relay(ctx context.Context, client, remote io.ReadWriteCloser, st *Stats) {
	if st != nil {
		st.TCP.Add(1)
		defer st.TCP.Add(-1)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = client.Close()
		_ = remote.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	copyDir := func(dst io.Writer, src io.Reader, counter *atomic.Uint64, closeTarget io.Closer) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if counter != nil {
					counter.Add(uint64(n))
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		if cw, ok := closeTarget.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = closeTarget.Close()
		}
	}

	var up, down *atomic.Uint64
	if st != nil {
		up = &st.BytesUp
		down = &st.BytesDown
	}
	go copyDir(remote, client, up, remote)
	go copyDir(client, remote, down, client)
	wg.Wait()
	_ = client.Close()
	_ = remote.Close()
}

type bufReadWriteCloser struct {
	net.Conn
	br *bufio.Reader
}

func (b *bufReadWriteCloser) Read(p []byte) (int, error) {
	if b.br != nil {
		n, err := b.br.Read(p)
		if b.br.Buffered() == 0 {
			b.br = nil
		}
		return n, err
	}
	return b.Conn.Read(p)
}
