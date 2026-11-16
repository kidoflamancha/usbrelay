package io.github.kidoflamancha.usbrelay

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.net.UnknownHostException
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import kotlin.concurrent.thread

class ProxyService : Service() {
    private val pool = Executors.newCachedThreadPool()
    private var activeSession: ProxySession? = null
    private var connectivity: ConnectivityManager? = null
    private var cellularCallback: ConnectivityManager.NetworkCallback? = null
    private var defaultCallback: ConnectivityManager.NetworkCallback? = null
    @Volatile private var cellularNetwork: Network? = null
    @Volatile private var defaultNetwork: Network? = null
    @Volatile private var lastHostSeenMs: Long = 0L

    private inner class ProxySession {
        val active = AtomicBoolean(true)
        @Volatile var server: ServerSocket? = null
        private var acceptThread: Thread? = null

        fun start() {
            acceptThread = thread(name = "usbrelay-accept", start = true) {
                runCatching { acceptLoop() }.onFailure {
                    if (active.get()) {
                        Log.e(TAG, "accept loop failed", it)
                        shutdownIfCurrent(this)
                    }
                }
            }
        }

        fun closeAndJoin() {
            active.set(false)
            runCatching { server?.close() }
            server = null
            val current = Thread.currentThread()
            if (acceptThread != null && acceptThread !== current) {
                runCatching { acceptThread?.join(1500) }
            }
        }

        private fun acceptLoop() {
            val srv = ServerSocket().apply {
                reuseAddress = true
                bind(InetSocketAddress(InetAddress.getByName("127.0.0.1"), LISTEN_PORT), 64)
            }
            server = srv
            srv.use {
                while (active.get()) {
                    val client = try {
                        srv.accept()
                    } catch (e: Exception) {
                        if (active.get()) throw e
                        break
                    }
                    pool.execute { handleClient(this, client) }
                }
            }
        }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                shutdown()
                return START_NOT_STICKY
            }
            ACTION_SET_MODE -> {
                val nextMode = intent.getIntExtra(EXTRA_NET_MODE, Prefs.NET_MODE_DEFAULT)
                netMode = nextMode
                Prefs.setNetMode(this, nextMode)
                updateNetworkCallbacks()
                publishState()
                return START_STICKY
            }
        }

        if (activeSession?.active?.get() == true) {
            return START_STICKY
        }

        netMode = Prefs.netMode(this)
        val notification = buildNotification(STATE_WAITING)
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(
                NOTIF_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE,
            )
        } else {
            startForeground(NOTIF_ID, notification)
        }

        connectivity = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        updateNetworkCallbacks()

        val session = startNewSession()
        publishIfCurrent(session, STATE_WAITING)
        session.start()
        Log.i(TAG, "proxy started on 127.0.0.1:$LISTEN_PORT mode=$netMode")
        return START_STICKY
    }

    override fun onDestroy() {
        shutdown()
        pool.shutdownNow()
        super.onDestroy()
    }

    @Synchronized
    private fun startNewSession(): ProxySession {
        activeSession?.closeAndJoin()
        val s = ProxySession()
        activeSession = s
        return s
    }

    @Synchronized
    private fun shutdownIfCurrent(session: ProxySession) {
        if (activeSession === session) {
            shutdown()
        }
    }

    @Synchronized
    private fun publishIfCurrent(session: ProxySession, next: Int) {
        if (activeSession === session) {
            state = next
            publishState()
        }
    }

    @Synchronized
    private fun shutdown() {
        stopNetworkCallbacks()
        activeSession?.closeAndJoin()
        activeSession = null
        activeConnections.set(0)
        state = STATE_OFF
        publishState()
        @Suppress("DEPRECATION")
        stopForeground(true)
        stopSelf()
        Log.i(TAG, "proxy stopped")
    }

    private fun updateNetworkCallbacks() {
        val cm = connectivity ?: return
        stopDefaultCallback(cm)
        registerDefaultCallback(cm)
        if (netMode == Prefs.NET_MODE_CELLULAR) {
            requestCellularNetwork(cm)
        } else {
            stopCellularCallback(cm)
        }
        netReady = isExitNetworkReady()
    }

    private fun registerDefaultCallback(cm: ConnectivityManager) {
        if (Build.VERSION.SDK_INT < 24) return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                defaultNetwork = network
                refreshNetReady()
            }

            override fun onLost(network: Network) {
                if (defaultNetwork == network) {
                    defaultNetwork = null
                }
                refreshNetReady()
            }
        }
        defaultCallback = cb
        runCatching { cm.registerDefaultNetworkCallback(cb) }
            .onFailure { Log.w(TAG, "registerDefaultNetworkCallback failed", it) }
    }

    private fun requestCellularNetwork(cm: ConnectivityManager) {
        if (cellularCallback != null) return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                cellularNetwork = network
                Log.i(TAG, "cellular network available: $network")
                refreshNetReady()
            }

            override fun onLost(network: Network) {
                if (cellularNetwork == network) {
                    cellularNetwork = null
                }
                Log.i(TAG, "cellular network lost")
                refreshNetReady()
            }

            override fun onUnavailable() {
                cellularNetwork = null
                refreshNetReady()
            }
        }
        cellularCallback = cb
        val req = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
            .build()
        runCatching { cm.requestNetwork(req, cb) }
            .onFailure { Log.w(TAG, "requestNetwork(CELLULAR) failed", it) }
    }

    private fun stopDefaultCallback(cm: ConnectivityManager) {
        defaultCallback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        defaultCallback = null
    }

    private fun stopCellularCallback(cm: ConnectivityManager) {
        cellularCallback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        cellularCallback = null
        cellularNetwork = null
    }

    private fun stopNetworkCallbacks() {
        val cm = connectivity ?: return
        stopDefaultCallback(cm)
        stopCellularCallback(cm)
        defaultNetwork = null
        connectivity = null
    }

    private fun refreshNetReady() {
        netReady = isExitNetworkReady()
        publishState()
    }

    private fun isExitNetworkReady(): Boolean {
        if (netMode == Prefs.NET_MODE_CELLULAR) {
            return cellularNetwork != null
        }
        if (defaultNetwork != null) return true
        val cm = connectivity ?: return false
        if (Build.VERSION.SDK_INT >= 23) {
            val active = cm.activeNetwork ?: return false
            val caps = cm.getNetworkCapabilities(active) ?: return false
            return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        }
        @Suppress("DEPRECATION")
        return cm.activeNetworkInfo?.isConnected == true
    }

    private fun handleClient(session: ProxySession, client: Socket) {
        try {
            client.tcpNoDelay = true
            client.soTimeout = 10_000
            val input = client.getInputStream()
            val output = client.getOutputStream()
            val cmd = input.read()
            if (cmd < 0) return

            lastHostSeenMs = System.currentTimeMillis()
            if (state == STATE_WAITING && session.active.get()) {
                publishIfCurrent(session, STATE_CONNECTED)
            }

            when (cmd) {
                CMD_PING -> handlePing(input, output)
                CMD_DIAL -> handleDial(session, client, input, output)
                else -> Log.w(TAG, "unknown command: $cmd")
            }
        } catch (e: Exception) {
            Log.d(TAG, "client error: ${e.message}")
        } finally {
            runCatching { client.close() }
        }
    }

    private fun handlePing(input: InputStream, output: OutputStream) {
        val ver = input.read()
        if (ver < 0) return
        val readyByte = if (isExitNetworkReady()) 1 else 0
        netReady = readyByte == 1
        output.write(
            byteArrayOf(
                CMD_PING.toByte(),
                PROTO_VERSION.toByte(),
                PLATFORM_ANDROID.toByte(),
                netMode.toByte(),
                readyByte.toByte(),
            )
        )
        output.flush()
    }

    private fun handleDial(
        session: ProxySession,
        client: Socket,
        input: InputStream,
        output: OutputStream,
    ) {
        val hostLen = input.read()
        if (hostLen <= 0) return
        val hostBytes = ByteArray(hostLen)
        readFully(input, hostBytes)
        val portHi = input.read()
        val portLo = input.read()
        if (portHi < 0 || portLo < 0) return
        val port = (portHi shl 8) or portLo
        val host = String(hostBytes, Charsets.UTF_8)

        val target = try {
            connectOutbound(host, port)
        } catch (e: NetworkUnavailableException) {
            output.write(STATUS_NET_UNAVAILABLE)
            output.flush()
            return
        } catch (e: UnknownHostException) {
            Log.d(TAG, "DNS failed for $host: ${e.message}")
            output.write(STATUS_DNS_FAILED)
            output.flush()
            return
        } catch (e: IOException) {
            Log.d(TAG, "connect failed $host:$port: ${e.message}")
            output.write(STATUS_CONNECT_FAILED)
            output.flush()
            return
        }

        client.soTimeout = 0
        output.write(STATUS_OK)
        output.flush()

        activeConnections.incrementAndGet()
        publishState()
        try {
            pumpBidirectional(session, client, target)
        } finally {
            activeConnections.decrementAndGet()
            runCatching { target.close() }
            publishState()
        }
    }

    private fun connectOutbound(host: String, port: Int): Socket {
        val network: Network? = if (netMode == Prefs.NET_MODE_CELLULAR) {
            cellularNetwork ?: throw NetworkUnavailableException()
        } else {
            defaultNetwork ?: if (Build.VERSION.SDK_INT >= 23) connectivity?.activeNetwork else null
        }

        val addrs = if (network != null) {
            network.getAllByName(host)
        } else {
            InetAddress.getAllByName(host)
        }
        if (addrs.isEmpty()) {
            throw UnknownHostException(host)
        }

        var lastErr: IOException? = null
        for (addr in addrs) {
            val s = if (network != null) {
                network.socketFactory.createSocket()
            } else {
                Socket()
            }
            try {
                s.tcpNoDelay = true
                s.connect(InetSocketAddress(addr, port), 10_000)
                return s
            } catch (e: IOException) {
                lastErr = e
                runCatching { s.close() }
            }
        }
        throw lastErr ?: IOException("failed to connect $host:$port")
    }

    private fun pumpBidirectional(session: ProxySession, a: Socket, b: Socket) {
        val t1 = thread(start = true, name = "relay-up") {
            copyStream(session, a.getInputStream(), b.getOutputStream())
            runCatching { b.shutdownOutput() }
        }
        copyStream(session, b.getInputStream(), a.getOutputStream())
        runCatching { a.shutdownOutput() }
        runCatching { t1.join(500) }
    }

    private fun copyStream(session: ProxySession, src: InputStream, dst: OutputStream) {
        val buf = ByteArray(32 * 1024)
        while (session.active.get()) {
            val n = try {
                src.read(buf)
            } catch (_: IOException) {
                break
            }
            if (n <= 0) break
            try {
                dst.write(buf, 0, n)
                dst.flush()
            } catch (_: IOException) {
                break
            }
        }
    }

    private fun readFully(input: InputStream, buf: ByteArray) {
        var off = 0
        while (off < buf.size) {
            val n = input.read(buf, off, buf.size - off)
            if (n < 0) throw EOFException()
            off += n
        }
    }

    private fun buildNotification(forState: Int = state): Notification {
        val channelId = "usbrelay"
        if (Build.VERSION.SDK_INT >= 26) {
            val mgr = getSystemService(NotificationManager::class.java)
            mgr.createNotificationChannel(
                NotificationChannel(channelId, getString(R.string.notif_channel), NotificationManager.IMPORTANCE_LOW)
            )
        }
        val pending = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        return NotificationCompat.Builder(this, channelId)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(statusText(forState))
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setContentIntent(pending)
            .setOngoing(true)
            .build()
    }

    private fun statusText(forState: Int): String = when (forState) {
        STATE_CONNECTED -> getString(R.string.notif_connected, activeConnections.get())
        STATE_WAITING -> getString(R.string.status_waiting_title)
        else -> getString(R.string.status_idle_title)
    }

    private fun publishState() {
        val mgr = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (state == STATE_OFF) {
            mgr.cancel(NOTIF_ID)
        } else {
            mgr.notify(NOTIF_ID, buildNotification(state))
        }
        sendBroadcast(
            Intent(ACTION_STATE)
                .setPackage(packageName)
                .putExtra(EXTRA_STATE, state),
        )
    }

    private class NetworkUnavailableException : IOException("exit network unavailable")

    companion object {
        const val LISTEN_PORT = 31416
        const val NOTIF_ID = 17
        const val ACTION_STATE = "io.github.kidoflamancha.usbrelay.STATE"
        const val ACTION_STOP = "io.github.kidoflamancha.usbrelay.STOP"
        const val ACTION_SET_MODE = "io.github.kidoflamancha.usbrelay.SET_MODE"
        const val EXTRA_STATE = "state"
        const val EXTRA_NET_MODE = "net_mode"

        const val STATE_OFF = 0
        const val STATE_WAITING = 1
        const val STATE_CONNECTED = 2

        private const val PROTO_VERSION = 1
        private const val CMD_PING = 0x01
        private const val CMD_DIAL = 0x02
        private const val PLATFORM_ANDROID = 0

        private const val STATUS_OK = 0x00
        private const val STATUS_DNS_FAILED = 0x01
        private const val STATUS_NET_UNAVAILABLE = 0x02
        private const val STATUS_CONNECT_FAILED = 0x03

        private const val TAG = "USBRelay"

        @Volatile
        var state: Int = STATE_OFF
            private set

        @Volatile
        var netMode: Int = Prefs.NET_MODE_DEFAULT
            private set

        @Volatile
        var netReady: Boolean = false
            private set

        val activeConnections = AtomicInteger(0)

        fun start(context: Context) {
            val intent = Intent(context, ProxyService::class.java)
            if (Build.VERSION.SDK_INT >= 26) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stop(context: Context) {
            context.startService(Intent(context, ProxyService::class.java).setAction(ACTION_STOP))
        }

        fun setNetworkMode(context: Context, mode: Int) {
            Prefs.setNetMode(context, mode)
            netMode = mode
            if (state != STATE_OFF) {
                context.startService(
                    Intent(context, ProxyService::class.java)
                        .setAction(ACTION_SET_MODE)
                        .putExtra(EXTRA_NET_MODE, mode)
                )
            }
        }
    }
}
