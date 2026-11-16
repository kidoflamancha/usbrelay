package transport

import (
	"context"
	"io"
)

type Platform uint8

const (
	Android Platform = 0
	IOS     Platform = 1
)

func (p Platform) String() string {
	switch p {
	case Android:
		return "Android"
	case IOS:
		return "iOS"
	default:
		return "unknown"
	}
}

type DeviceEvent struct {
	ID       string // adb serial or usbmux DeviceID
	Serial   string // device serial / UDID
	Name     string // human-readable device model or name
	Platform Platform
	Attached bool
	Message  string // optional status hint, e.g. unauthorized
}

// DeviceDialer opens fresh byte streams to a specific listening port on a connected device.
type DeviceDialer interface {
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
	Close() error
}

type Transport interface {
	Watch(ctx context.Context) (<-chan DeviceEvent, error)
	Open(ctx context.Context, deviceID string, port uint16) (DeviceDialer, error)
}
