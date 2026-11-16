package proto

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func TestPingRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		var req [2]byte
		if _, err := io.ReadFull(b, req[:]); err != nil {
			return
		}
		if req[0] != CmdPing || req[1] != Version {
			return
		}
		_, _ = b.Write([]byte{CmdPing, Version, 0, NetModeCellular, 1})
	}()

	info, err := Ping(a)
	if err != nil {
		t.Fatal(err)
	}
	if info.NetMode != NetModeCellular || !info.NetReady || info.NetModeString() != "cellular" {
		t.Fatalf("unexpected PingInfo: %+v", info)
	}
}

func TestDialRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		var hdr [2]byte
		if _, err := io.ReadFull(b, hdr[:]); err != nil {
			return
		}
		host := make([]byte, hdr[1])
		if _, err := io.ReadFull(b, host); err != nil {
			return
		}
		var portBuf [2]byte
		if _, err := io.ReadFull(b, portBuf[:]); err != nil {
			return
		}
		port := binary.BigEndian.Uint16(portBuf[:])
		if hdr[0] == CmdDial && string(host) == "example.com" && port == 443 {
			_, _ = b.Write([]byte{StatusOK})
		} else {
			_, _ = b.Write([]byte{StatusConnectFailed})
		}
	}()

	if err := Dial(a, "example.com", 443); err != nil {
		t.Fatal(err)
	}
}
