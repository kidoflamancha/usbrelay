package sysproxy

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type savedState struct {
	service string
	web     proxySetting
	secure  proxySetting
	socks   proxySetting
}

type proxySetting struct {
	enabled bool
	server  string
	port    string
}

// Enable configures the active macOS network service to route HTTP, HTTPS, and SOCKS5
// traffic through host:port, returning a restore function that reverts to the prior state.
func Enable(host string, port uint16) (func(), error) {
	if runtime.GOOS != "darwin" {
		return func() {}, fmt.Errorf("sysproxy: automatic system proxy is only supported on macOS")
	}
	svc, err := activeNetworkService()
	if err != nil {
		return func() {}, err
	}
	saved := savedState{
		service: svc,
		web:     readProxy(svc, "-getwebproxy"),
		secure:  readProxy(svc, "-getsecurewebproxy"),
		socks:   readProxy(svc, "-getsocksfirewallproxy"),
	}
	portStr := strconv.Itoa(int(port))
	cmds := [][]string{
		{"-setwebproxy", svc, host, portStr},
		{"-setwebproxystate", svc, "on"},
		{"-setsecurewebproxy", svc, host, portStr},
		{"-setsecurewebproxystate", svc, "on"},
		{"-setsocksfirewallproxy", svc, host, portStr},
		{"-setsocksfirewallproxystate", svc, "on"},
	}
	for _, args := range cmds {
		if out, err := exec.Command("networksetup", args...).CombinedOutput(); err != nil {
			restoreState(saved)
			return func() {}, fmt.Errorf("networksetup %v: %v (%s)", args, err, strings.TrimSpace(string(out)))
		}
	}
	return func() { restoreState(saved) }, nil
}

func restoreState(s savedState) {
	apply := func(setCmd, stateCmd string, p proxySetting) {
		if p.server != "" && p.port != "" && p.port != "0" {
			_ = exec.Command("networksetup", setCmd, s.service, p.server, p.port).Run()
		}
		state := "off"
		if p.enabled {
			state = "on"
		}
		_ = exec.Command("networksetup", stateCmd, s.service, state).Run()
	}
	apply("-setwebproxy", "-setwebproxystate", s.web)
	apply("-setsecurewebproxy", "-setsecurewebproxystate", s.secure)
	apply("-setsocksfirewallproxy", "-setsocksfirewallproxystate", s.socks)
}

func readProxy(service, flag string) proxySetting {
	out, err := exec.Command("networksetup", flag, service).CombinedOutput()
	if err != nil {
		return proxySetting{}
	}
	var p proxySetting
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Enabled":
			p.enabled = strings.EqualFold(v, "Yes")
		case "Server":
			p.server = v
		case "Port":
			p.port = v
		}
	}
	return p
}

func activeNetworkService() (string, error) {
	// Resolve default route device (e.g. en0), falling back to the first enabled service.
	var defaultDev string
	if out, err := exec.Command("route", "-n", "get", "default").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.TrimSpace(k) == "interface" {
				defaultDev = strings.TrimSpace(v)
				break
			}
		}
	}
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("networksetup -listnetworkserviceorder: %w", err)
	}
	var currentSvc string
	var fallback string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(Hardware Port") {
			if idx := strings.Index(line, ") "); idx > 0 {
				name := strings.TrimSpace(line[idx+2:])
				if !strings.HasPrefix(name, "*") {
					currentSvc = name
					if fallback == "" {
						fallback = name
					}
				} else {
					currentSvc = ""
				}
			}
			continue
		}
		if currentSvc != "" && defaultDev != "" && strings.Contains(line, "Device: "+defaultDev+")") {
			return currentSvc, nil
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("sysproxy: no active network service found")
}
