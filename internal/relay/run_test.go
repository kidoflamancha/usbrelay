package relay

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"usbrelay/internal/proto"
	"usbrelay/internal/transport"
	"usbrelay/internal/ui"
)

type fakeTransport struct {
	watchCalls atomic.Int32
	httpTarget string
}

func (f *fakeTransport) Watch(ctx context.Context) (<-chan transport.DeviceEvent, error) {
	n := f.watchCalls.Add(1)
	ch := make(chan transport.DeviceEvent, 2)
	if n == 1 {
		close(ch)
		return ch, nil
	}
	ch <- transport.DeviceEvent{
		ID:       "dev-1",
		Serial:   "dev-1",
		Name:     "Test Phone",
		Platform: transport.Android,
		Attached: true,
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f *fakeTransport) Open(_ context.Context, _ string, _ uint16) (transport.DeviceDialer, error) {
	return &fakeDeviceDialer{httpTarget: f.httpTarget}, nil
}

type fakeDeviceDialer struct {
	httpTarget string
}

func (d *fakeDeviceDialer) Dial(_ context.Context) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	go d.servePhoneStream(a)
	return b, nil
}

func (d *fakeDeviceDialer) Close() error { return nil }

func (d *fakeDeviceDialer) servePhoneStream(conn net.Conn) {
	defer conn.Close()
	var cmd [1]byte
	if _, err := io.ReadFull(conn, cmd[:]); err != nil {
		return
	}
	switch cmd[0] {
	case proto.CmdPing:
		var ver [1]byte
		if _, err := io.ReadFull(conn, ver[:]); err != nil {
			return
		}
		_, _ = conn.Write([]byte{proto.CmdPing, proto.Version, 0, proto.NetModeCellular, 1})
	case proto.CmdDial:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return
		}
		host := make([]byte, l[0])
		if _, err := io.ReadFull(conn, host); err != nil {
			return
		}
		var pb [2]byte
		if _, err := io.ReadFull(conn, pb[:]); err != nil {
			return
		}
		port := binary.BigEndian.Uint16(pb[:])
		target, err := net.Dial("tcp", net.JoinHostPort(string(host), strconv.Itoa(int(port))))
		if err != nil {
			_, _ = conn.Write([]byte{proto.StatusConnectFailed})
			return
		}
		defer target.Close()
		_, _ = conn.Write([]byte{proto.StatusOK})
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, conn)
			close(done)
		}()
		_, _ = io.Copy(conn, target)
		<-done
	}
}

func TestRunWatchReconnectAndEndToEndPhoneProxy(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("cellular-ok"))
		}),
	}
	go func() { _ = srv.Serve(echoLn) }()
	defer srv.Close()

	tr := &fakeTransport{httpTarget: echoLn.Addr().String()}
	boundCh := make(chan string, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			ListenAddr:     "127.0.0.1:0",
			Transport:      tr,
			WatchRetryWait: 20 * time.Millisecond,
			Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			Display:        ui.New(io.Discard),
			OnProxyBound: func(addr string) {
				select {
				case boundCh <- addr:
				default:
				}
			},
		})
	}()

	var proxyAddr string
	select {
	case proxyAddr = <-boundCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy bind")
	}

	pURL, _ := url.Parse("http://" + proxyAddr)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pURL)},
		Timeout:   2 * time.Second,
	}

	var body []byte
	for i := 0; i < 40; i++ {
		resp, err := client.Get("http://" + echoLn.Addr().String() + "/ping")
		if err == nil && resp.StatusCode == http.StatusOK {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(25 * time.Millisecond)
	}
	if string(body) != "cellular-ok" {
		t.Fatalf("expected cellular-ok through phone proxy, got %q", body)
	}
	if tr.watchCalls.Load() < 2 {
		t.Fatalf("expected watch retry, got %d", tr.watchCalls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}
