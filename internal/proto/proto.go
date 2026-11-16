package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version byte = 1

	CmdPing byte = 0x01
	CmdDial byte = 0x02

	NetModeDefault  byte = 0
	NetModeCellular byte = 1

	StatusOK             byte = 0x00
	StatusDNSFailed      byte = 0x01
	StatusNetUnavailable byte = 0x02
	StatusConnectFailed  byte = 0x03
)

var ErrProtocol = errors.New("proto: invalid response")

// PingInfo describes the status returned by the device app for CmdPing.
type PingInfo struct {
	Version  byte
	Platform byte
	NetMode  byte
	NetReady bool
}

func (p PingInfo) NetModeString() string {
	switch p.NetMode {
	case NetModeCellular:
		return "cellular"
	default:
		return "default"
	}
}

// Ping sends CmdPing over stream and parses the device status response.
func Ping(rw io.ReadWriter) (PingInfo, error) {
	if _, err := rw.Write([]byte{CmdPing, Version}); err != nil {
		return PingInfo{}, err
	}
	var resp [5]byte
	if _, err := io.ReadFull(rw, resp[:]); err != nil {
		return PingInfo{}, err
	}
	if resp[0] != CmdPing {
		return PingInfo{}, fmt.Errorf("%w: ping cmd=0x%02x", ErrProtocol, resp[0])
	}
	if resp[1] != Version {
		return PingInfo{}, fmt.Errorf("%w: version mismatch (device=%d host=%d)", ErrProtocol, resp[1], Version)
	}
	return PingInfo{
		Version:  resp[1],
		Platform: resp[2],
		NetMode:  resp[3],
		NetReady: resp[4] != 0,
	}, nil
}

// Dial requests the device app to open an outbound TCP connection to host:port.
func Dial(rw io.ReadWriter, host string, port uint16) error {
	if len(host) == 0 || len(host) > 255 {
		return fmt.Errorf("proto: invalid host length %d", len(host))
	}
	buf := make([]byte, 4+len(host))
	buf[0] = CmdDial
	buf[1] = byte(len(host))
	copy(buf[2:], host)
	binary.BigEndian.PutUint16(buf[2+len(host):], port)

	if _, err := rw.Write(buf); err != nil {
		return err
	}
	var status [1]byte
	if _, err := io.ReadFull(rw, status[:]); err != nil {
		return err
	}
	switch status[0] {
	case StatusOK:
		return nil
	case StatusDNSFailed:
		return fmt.Errorf("device DNS resolution failed for %s", host)
	case StatusNetUnavailable:
		return fmt.Errorf("device exit network unavailable")
	case StatusConnectFailed:
		return fmt.Errorf("device failed to connect %s:%d", host, port)
	default:
		return fmt.Errorf("device dial error code 0x%02x", status[0])
	}
}
