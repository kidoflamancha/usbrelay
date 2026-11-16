package ui

import (
	"bytes"
	"strings"
	"testing"

	"usbrelay/internal/proxy"
	"usbrelay/internal/transport"
)

func TestDisplayRenderAndStatsReset(t *testing.T) {
	var buf bytes.Buffer
	d := New(&buf)
	d.Render()
	if !strings.Contains(buf.String(), "waiting for USB devices") {
		t.Fatalf("unexpected empty render: %q", buf.String())
	}

	st := &proxy.Stats{}
	st.BytesUp.Store(2048)
	d.Upsert(Device{
		ID:       "ser1",
		Serial:   "ser1",
		Name:     "Pixel 8",
		Platform: transport.Android,
		State:    StateConnected,
		NetMode:  "cellular",
		Stats:    st,
	})
	views := d.Snapshot()
	if len(views) != 1 || views[0].UpBps != 0 || !strings.Contains(views[0].Line(), "connected (cellular)") {
		t.Fatalf("unexpected first snapshot: %+v", views)
	}

	st2 := &proxy.Stats{}
	st2.BytesUp.Store(100)
	d.SetStats("ser1", st2)
	views = d.Snapshot()
	if views[0].UpBps != 0 {
		t.Fatalf("expected 0 bps after stats reset, got %v", views[0].UpBps)
	}
}
