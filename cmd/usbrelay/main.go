package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"usbrelay/internal/relay"
	"usbrelay/internal/sysproxy"
	"usbrelay/internal/transport/adb"
	"usbrelay/internal/ui"
)

var version = "0.2.0-dev"

const defaultAPKRelPath = "android/app/build/outputs/apk/debug/app-debug.apk"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: usbrelay <run|devices|install|version>")
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "devices":
		return cmdDevices(args[1:])
	case "install":
		return cmdInstall(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("usbrelay %s\n", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (expected: run, devices, install, version)", args[0])
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:1080", "local mixed SOCKS5/HTTP proxy listen address")
	device := fs.String("device", "", "only use this adb serial")
	setSysProxy := fs.Bool("sysproxy", false, "set macOS system HTTP/HTTPS/SOCKS proxy while running (restored on exit)")
	verbose := fs.Bool("verbose", false, "log each proxied connection")
	if err := fs.Parse(args); err != nil {
		return err
	}

	host, portStr, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid --listen address %q: %w", *listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --listen port %q", portStr)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	log.Info("usbrelay proxy starting", "listen", *listen, "sysproxy", *setSysProxy)

	if *setSysProxy {
		restore, err := sysproxy.Enable(host, uint16(port))
		if err != nil {
			return fmt.Errorf("enable system proxy: %w", err)
		}
		defer restore()
		log.Info("macOS system proxy enabled", "host", host, "port", port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	disp := newDisplay(ctx)
	return relay.Run(ctx, relay.Config{
		ListenAddr: *listen,
		Device:     *device,
		Log:        log,
		Display:    disp,
	})
}

func newDisplay(ctx context.Context) *ui.Display {
	disp := ui.New(os.Stderr)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				disp.Render()
			}
		}
	}()
	return disp
}

func cmdDevices(args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	list, err := adb.New().ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("adb: %w", err)
	}
	if len(list) == 0 {
		fmt.Println("Android: (none)")
		return nil
	}
	for _, d := range list {
		name := d.Name
		if name == "" {
			name = d.Serial
		}
		fmt.Printf("Android  %s  %s\n", name, d.Serial)
	}
	return nil
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	device := fs.String("device", "", "adb serial")
	apkPath := fs.String("apk", "", "path to APK file (defaults to android debug APK if present)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved := *apkPath
	if resolved == "" {
		candidates := []string{defaultAPKRelPath}
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", defaultAPKRelPath))
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				resolved = c
				break
			}
		}
	}
	if resolved == "" {
		return fmt.Errorf("APK not found; pass --apk <path> or build %s first", defaultAPKRelPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fmt.Printf("Installing %s...\n", resolved)
	if err := adb.InstallAPK(ctx, *device, resolved); err != nil {
		return err
	}
	fmt.Println("Install succeeded.")
	return nil
}
