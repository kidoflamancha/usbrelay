package ui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"usbrelay/internal/proxy"
	"usbrelay/internal/transport"
)

type State string

const (
	StateWaiting      State = "waiting for app"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateError        State = "error"
	maxLogs                 = 12
)

type Device struct {
	ID       string
	Serial   string
	Name     string
	Platform transport.Platform
	State    State
	Detail   string
	NetMode  string
	Stats    *proxy.Stats
}

type DeviceView struct {
	ID       string             `json:"id"`
	Serial   string             `json:"serial"`
	Name     string             `json:"name"`
	Platform transport.Platform `json:"platform"`
	State    State              `json:"state"`
	Detail   string             `json:"detail"`
	NetMode  string             `json:"net_mode"`
	TCP      int64              `json:"tcp"`
	UpBps    float64            `json:"up_bps"`
	DownBps  float64            `json:"down_bps"`
}

func (v DeviceView) Line() string {
	label := v.Name
	if label == "" {
		label = v.Serial
		if label == "" {
			label = v.ID
		}
		label = trimSerial(label)
	}
	stateText := string(v.State)
	if v.State == StateConnected && v.NetMode != "" {
		stateText = fmt.Sprintf("%s (%s)", v.State, v.NetMode)
	}
	line := fmt.Sprintf("[%s %s]  %s  tcp:%d  ↑%s ↓%s",
		v.Platform, label,
		stateText, v.TCP,
		formatRate(v.UpBps), formatRate(v.DownBps),
	)
	if v.Detail != "" && v.State != StateConnected {
		line += "  " + v.Detail
	}
	return line
}

type Display struct {
	mu      sync.Mutex
	out     io.Writer
	devices map[string]*Device
	prev    map[string]snap
	logs    []string
	lines   int
}

type snap struct {
	up, down uint64
	at       time.Time
}

func New(out io.Writer) *Display {
	if out == nil {
		out = os.Stderr
	}
	return &Display{
		out:     out,
		devices: map[string]*Device{},
		prev:    map[string]snap{},
	}
}

func (d *Display) Upsert(dev Device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := dev
	d.devices[dev.ID] = &cp
}

func (d *Display) Remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.devices, id)
	delete(d.prev, id)
}

func (d *Display) SetState(id string, st State, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev, ok := d.devices[id]; ok {
		dev.State = st
		dev.Detail = detail
	}
}

func (d *Display) SetNetMode(id, mode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev, ok := d.devices[id]; ok {
		dev.NetMode = mode
	}
}

func (d *Display) SetStats(id string, st *proxy.Stats) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev, ok := d.devices[id]; ok {
		dev.Stats = st
		delete(d.prev, id)
	}
}

func (d *Display) Snapshot() []DeviceView {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotLocked(time.Now())
}

func (d *Display) Render() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lines > 0 {
		fmt.Fprintf(d.out, "\x1b[%dA\x1b[J", d.lines)
	}
	views := d.snapshotLocked(time.Now())
	if len(views) == 0 {
		fmt.Fprintln(d.out, "waiting for USB devices…")
		d.lines = 1
		return
	}
	for _, v := range views {
		fmt.Fprintln(d.out, v.Line())
	}
	d.lines = len(views)
}

func (d *Display) snapshotLocked(now time.Time) []DeviceView {
	views := make([]DeviceView, 0, len(d.devices))
	for _, dev := range d.devices {
		tcp, up, down := int64(0), uint64(0), uint64(0)
		if dev.Stats != nil {
			tcp, up, down = dev.Stats.Snapshot()
		}
		upBps, downBps := 0.0, 0.0
		if p, ok := d.prev[dev.ID]; ok {
			dt := now.Sub(p.at).Seconds()
			if dt > 0 && up >= p.up && down >= p.down {
				upBps = float64(up-p.up) / dt
				downBps = float64(down-p.down) / dt
			}
		}
		d.prev[dev.ID] = snap{up: up, down: down, at: now}
		views = append(views, DeviceView{
			ID:       dev.ID,
			Serial:   dev.Serial,
			Name:     dev.Name,
			Platform: dev.Platform,
			State:    dev.State,
			Detail:   dev.Detail,
			NetMode:  dev.NetMode,
			TCP:      tcp,
			UpBps:    upBps,
			DownBps:  downBps,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Platform != views[j].Platform {
			return views[i].Platform > views[j].Platform
		}
		si, sj := views[i].Serial, views[j].Serial
		if si == "" {
			si = views[i].ID
		}
		if sj == "" {
			sj = views[j].ID
		}
		return si < sj
	})
	return views
}

func trimSerial(s string) string {
	if len(s) > 20 {
		return s[:20] + "…"
	}
	return s
}

func formatRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	switch {
	case bps >= 1024*1024:
		return fmt.Sprintf("%.1fMB/s", bps/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.0fKB/s", bps/1024)
	default:
		return fmt.Sprintf("%.0fB/s", bps)
	}
}

func (d *Display) Logf(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lines > 0 {
		fmt.Fprintf(d.out, "\x1b[%dA\x1b[J", d.lines)
		d.lines = 0
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(d.out, msg)
	d.logs = append(d.logs, time.Now().Format("15:04:05")+" "+msg)
	if len(d.logs) > maxLogs {
		d.logs = d.logs[len(d.logs)-maxLogs:]
	}
}
