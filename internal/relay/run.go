package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"usbrelay/internal/proto"
	"usbrelay/internal/proxy"
	"usbrelay/internal/transport"
	"usbrelay/internal/transport/adb"
	"usbrelay/internal/ui"
)

const (
	ListenPort      uint16 = 31416
	watchRetryDelay        = 2 * time.Second
	probeRetryWait         = 2 * time.Second
	probeKeepWait          = 5 * time.Second
)

type Config struct {
	ListenAddr     string
	Device         string
	Transport      transport.Transport // optional; defaults to adb.New()
	WatchRetryWait time.Duration       // optional; defaults to 2s
	Log            *slog.Logger
	Display        *ui.Display
	OnProxyBound   func(addr string) // optional hook when proxy listener binds
}

type deviceDialerAdapter struct {
	sess transport.DeviceDialer
}

func (a *deviceDialerAdapter) DialTarget(ctx context.Context, host string, port uint16) (io.ReadWriteCloser, error) {
	stream, err := a.sess.Dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	err = proto.Dial(stream, host, port)
	stop()
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

func Run(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:1080"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Display == nil {
		cfg.Display = ui.New(io.Discard)
	}
	if cfg.WatchRetryWait <= 0 {
		cfg.WatchRetryWait = watchRetryDelay
	}

	tr := cfg.Transport
	if tr == nil {
		tr = adb.New()
	}

	srv := proxy.NewServer(cfg.ListenAddr, cfg.Log)
	reg := &backendRegistry{srv: srv}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return srv.ListenAndServe(ctx)
	})
	if cfg.OnProxyBound != nil {
		go func() {
			for i := 0; i < 100 && ctx.Err() == nil; i++ {
				addr := srv.Addr()
				if addr != "" && addr != "127.0.0.1:0" {
					cfg.OnProxyBound(addr)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	g.Go(func() error {
		return runWatchAndDevices(ctx, tr, cfg, reg)
	})

	err := g.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func runWatchAndDevices(ctx context.Context, tr transport.Transport, cfg Config, reg *backendRegistry) error {
	events := make(chan transport.DeviceEvent, 32)
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		defer close(events)
		watchLoop(ctx, "adb", tr, cfg.WatchRetryWait, cfg.Log, events)
	}()

	type running struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	loops := map[string]running{}
	stopDevice := func(id string) {
		reg.Remove(id)
		if r, ok := loops[id]; ok {
			r.cancel()
			<-r.done
			delete(loops, id)
		}
		cfg.Display.Remove(id)
	}
	defer func() {
		for _, r := range loops {
			r.cancel()
		}
		for id := range loops {
			stopDevice(id)
		}
		watchWG.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if cfg.Device != "" && ev.ID != cfg.Device && ev.Serial != cfg.Device {
				continue
			}
			if ev.Message != "" {
				cfg.Display.Logf("%s %s: %s", ev.Platform, ev.Serial, ev.Message)
			}
			if ev.Attached {
				if _, exists := loops[ev.ID]; exists {
					continue
				}
				dctx, dcancel := context.WithCancel(ctx)
				done := make(chan struct{})
				loops[ev.ID] = running{cancel: dcancel, done: done}
				cfg.Display.Upsert(ui.Device{
					ID:       ev.ID,
					Serial:   ev.Serial,
					Name:     ev.Name,
					Platform: ev.Platform,
					State:    ui.StateWaiting,
				})
				go deviceLoop(dctx, ev, tr, cfg, reg, func() { close(done) })
			} else {
				stopDevice(ev.ID)
			}
		}
	}
}

func watchLoop(ctx context.Context, name string, tr transport.Transport, retryWait time.Duration, log *slog.Logger, out chan<- transport.DeviceEvent) {
	active := map[string]transport.DeviceEvent{}
	emitDetachAll := func() {
		for id, ev := range active {
			ev.Attached = false
			ev.Message = ""
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
			delete(active, id)
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}
		ch, err := tr.Watch(ctx)
		if err != nil {
			log.Warn("transport watch failed, retrying", "name", name, "err", err)
		} else {
			for ev := range ch {
				if ev.Attached {
					active[ev.ID] = ev
				} else {
					delete(active, ev.ID)
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
			log.Warn("transport watch stream ended, reconnecting", "name", name)
			emitDetachAll()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryWait):
		}
	}
}

func deviceLoop(ctx context.Context, ev transport.DeviceEvent, tr transport.Transport, cfg Config, reg *backendRegistry, done func()) {
	defer done()
	defer reg.Remove(ev.ID)

	for {
		if ctx.Err() != nil {
			return
		}
		cfg.Display.SetState(ev.ID, ui.StateWaiting, fmt.Sprintf("setting up usb forward (port %d)", ListenPort))
		sess, err := tr.Open(ctx, ev.ID, ListenPort)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			cfg.Log.Debug("open transport failed", "device", ev.Serial, "err", err)
			cfg.Display.SetState(ev.ID, ui.StateWaiting, err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeRetryWait):
			}
			continue
		}

		st := &proxy.Stats{}
		adapter := &deviceDialerAdapter{sess: sess}
		registered := false

		for ctx.Err() == nil {
			info, err := probeOnce(ctx, sess)
			if err != nil {
				if registered {
					reg.Remove(ev.ID)
					registered = false
				}
				if ctx.Err() != nil {
					break
				}
				cfg.Log.Debug("device probe failed", "device", ev.Serial, "err", err)
				cfg.Display.SetState(ev.ID, ui.StateWaiting, fmt.Sprintf("open USB Relay app on device (port %d)", ListenPort))
				select {
				case <-ctx.Done():
				case <-time.After(probeRetryWait):
				}
				continue
			}

			cfg.Display.SetNetMode(ev.ID, info.NetModeString())
			if !info.NetReady {
				if registered {
					reg.Remove(ev.ID)
					registered = false
				}
				cfg.Display.SetState(ev.ID, ui.StateWaiting, fmt.Sprintf("exit network unavailable (%s)", info.NetModeString()))
			} else {
				if !registered {
					cfg.Display.SetStats(ev.ID, st)
					reg.Set(ev.ID, adapter, st)
					registered = true
					cfg.Log.Info("device ready for proxy", "device", ev.Serial, "net_mode", info.NetModeString())
				}
				cfg.Display.SetState(ev.ID, ui.StateConnected, "")
			}

			select {
			case <-ctx.Done():
			case <-time.After(probeKeepWait):
			}
		}

		_ = sess.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

func probeOnce(ctx context.Context, sess transport.DeviceDialer) (proto.PingInfo, error) {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream, err := sess.Dial(pctx)
	if err != nil {
		return proto.PingInfo{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(pctx, func() { _ = stream.Close() })
	defer stop()
	return proto.Ping(stream)
}

type backendEntry struct {
	id     string
	dialer proxy.TargetDialer
	stats  *proxy.Stats
}

type backendRegistry struct {
	mu      sync.Mutex
	srv     *proxy.Server
	entries []backendEntry
}

func (r *backendRegistry) Set(id string, d proxy.TargetDialer, st *proxy.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.entries {
		if r.entries[i].id == id {
			r.entries[i] = backendEntry{id: id, dialer: d, stats: st}
			r.syncLocked()
			return
		}
	}
	r.entries = append(r.entries, backendEntry{id: id, dialer: d, stats: st})
	r.syncLocked()
}

func (r *backendRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.entries {
		if r.entries[i].id == id {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			r.syncLocked()
			return
		}
	}
}

func (r *backendRegistry) syncLocked() {
	if len(r.entries) == 0 {
		r.srv.SetBackend(nil, nil)
		return
	}
	first := r.entries[0]
	r.srv.SetBackend(first.dialer, first.stats)
}
