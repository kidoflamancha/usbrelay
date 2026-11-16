# usbrelay

Share your mobile device's network (Wi-Fi or forced 4G/5G cellular) with your computer as a local SOCKS5 + HTTP/HTTPS proxy over USB — **no root or VPN permission required**.

Currently **Android** is implemented. iOS (`usbmuxd` + `NWConnection` cellular binding) is planned for Phase 2.

## Features

- **No Root / No VPN Permission**: Android companion app runs as a standard `connectedDevice` foreground service listening on `127.0.0.1:31416` via `adb forward`.
- **Mixed SOCKS5 & HTTP/HTTPS Proxy**: A single local port on your computer (`127.0.0.1:1080` by default) handles both `socks5://` / `socks5h://` (remote DNS on phone) and `http://` (HTTP & HTTPS `CONNECT`).
- **Force Cellular Exit (4G/5G)**: Toggle between **Auto (Default)** and **Cellular Only (4G/5G)** in the phone app (`ConnectivityManager.requestNetwork(TRANSPORT_CELLULAR)`), allowing your computer to use cellular data even while the phone remains on Wi-Fi.
- **macOS System Proxy Support**: Pass `--sysproxy` to automatically configure HTTP/HTTPS/SOCKS system proxies on macOS and restore previous settings on exit.
- **Resilient ADB Watcher**: Automatically reconnects if the ADB server restarts or devices are replugged.

## Requirements

- **Desktop**: Go 1.27+, `adb` in `PATH`
- **Android**: Android 5.0 (API 21)+ with USB debugging enabled (build APK with JDK 17 + Android SDK 34 or Android Studio)

## Quick Start

### 1. Build Desktop CLI

```bash
go build -o bin/usbrelay ./cmd/usbrelay
```

### 2. Build & Install Android App

Open `android/` in Android Studio, or build via Gradle (JDK 17 + Android SDK 34):

```bash
cd android
./gradlew assembleDebug
cd ..
./bin/usbrelay install
```

### 3. Share Phone Network to Computer

1. Open **USB Relay** on your Android device. Optionally tap **Exit network** to switch between **Auto (Default)** and **Cellular Only (4G/5G)**.
2. Connect the phone over USB and authorize USB debugging.
3. Start the proxy on your computer:

```bash
# Mixed SOCKS5 + HTTP/HTTPS proxy on 127.0.0.1:1080
./bin/usbrelay run

# Automatically configure macOS system proxy (restored on Ctrl+C)
./bin/usbrelay run --sysproxy

# Custom listen address, device filter, and verbose logs
./bin/usbrelay run --listen 127.0.0.1:10808 --device <serial> --verbose
```

4. Point any tool to `127.0.0.1:1080`:

```bash
curl -x socks5h://127.0.0.1:1080 https://ifconfig.me
curl -x http://127.0.0.1:1080 https://example.com
```

## Development Checks

```bash
go test -race -timeout 60s ./...
go vet ./...
cd android && ./gradlew assembleDebug lintDebug
```

## License

[MIT](LICENSE)
