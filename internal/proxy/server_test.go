package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

type loopbackDialer struct {
	t *testing.T
}

func (l *loopbackDialer) DialTarget(ctx context.Context, host string, port uint16) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
}

func TestMixedSOCKS5AndHTTPProxy(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("via-phone-http"))
		}),
	}
	go func() { _ = httpSrv.Serve(echoLn) }()
	defer httpSrv.Close()

	srv := NewServer("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := &Stats{}
	srv.SetBackend(&loopbackDialer{t: t}, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	// Wait briefly until listener is bound.
	var proxyAddr string
	for i := 0; i < 50; i++ {
		proxyAddr = srv.Addr()
		if proxyAddr != "127.0.0.1:0" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1. Test HTTP proxy mode (plain HTTP).
	pURL, _ := url.Parse("http://" + proxyAddr)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pURL)},
		Timeout:   2 * time.Second,
	}
	resp, err := client.Get("http://" + echoLn.Addr().String() + "/test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via-phone-http" {
		t.Fatalf("unexpected HTTP proxy body: %q", body)
	}

	// 2. Test HTTP CONNECT tunneling.
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("CONNECT " + echoLn.Addr().String() + " HTTP/1.1\r\nHost: " + echoLn.Addr().String() + "\r\n\r\n"))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || line[:12] != "HTTP/1.1 200" {
		t.Fatalf("CONNECT response: %q err=%v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
	connectBody, _ := io.ReadAll(br)
	conn.Close()
	if !contains(string(connectBody), "via-phone-http") {
		t.Fatalf("CONNECT body missing payload: %q", connectBody)
	}

	// 3. Test SOCKS5 CONNECT mode.
	sc, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	_, _ = sc.Write([]byte{0x05, 0x01, 0x00})
	var m [2]byte
	if _, err := io.ReadFull(sc, m[:]); err != nil || m != [2]byte{0x05, 0x00} {
		t.Fatalf("socks5 method reply=%v err=%v", m, err)
	}
	h, pStr, _ := net.SplitHostPort(echoLn.Addr().String())
	p, _ := strconv.Atoi(pStr)
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(h))}, h...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(p))
	req = append(req, pb[:]...)
	_, _ = sc.Write(req)
	var rep [10]byte
	if _, err := io.ReadFull(sc, rep[:]); err != nil || rep[1] != 0x00 {
		t.Fatalf("socks5 connect reply=%v err=%v", rep, err)
	}
	_, _ = sc.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
	socksBody, _ := io.ReadAll(sc)
	if !contains(string(socksBody), "via-phone-http") {
		t.Fatalf("socks5 body missing payload: %q", socksBody)
	}

	if _, up, down := st.Snapshot(); up == 0 || down == 0 {
		t.Fatalf("expected non-zero byte counters, got up=%d down=%d", up, down)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
