package io.github.kidoflamancha.usbrelay

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Bundle
import android.view.View
import android.view.WindowManager
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import com.google.android.material.button.MaterialButton

class MainActivity : AppCompatActivity() {
    private lateinit var statusTitle: TextView
    private lateinit var statusDetail: TextView
    private lateinit var statusDot: ImageView
    private lateinit var steps: LinearLayout
    private lateinit var btnPrimary: MaterialButton
    private lateinit var btnStop: MaterialButton
    private lateinit var btnNetMode: MaterialButton

    private val receiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            render()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        applyLightSystemBars()
        statusTitle = findViewById(R.id.statusTitle)
        statusDetail = findViewById(R.id.statusDetail)
        statusDot = findViewById(R.id.statusDot)
        steps = findViewById(R.id.steps)
        btnPrimary = findViewById(R.id.btnPrimary)
        btnStop = findViewById(R.id.btnStop)
        btnNetMode = findViewById(R.id.btnNetMode)

        btnPrimary.setOnClickListener {
            ProxyService.start(this)
            render()
        }
        btnStop.setOnClickListener {
            ProxyService.stop(this)
            render()
        }
        btnNetMode.setOnClickListener {
            val current = Prefs.netMode(this)
            val next = if (current == Prefs.NET_MODE_CELLULAR) {
                Prefs.NET_MODE_DEFAULT
            } else {
                Prefs.NET_MODE_CELLULAR
            }
            ProxyService.setNetworkMode(this, next)
            render()
        }

        ProxyService.start(this)
    }

    override fun onResume() {
        super.onResume()
        ContextCompat.registerReceiver(
            this,
            receiver,
            IntentFilter(ProxyService.ACTION_STATE),
            ContextCompat.RECEIVER_NOT_EXPORTED,
        )
        render()
    }

    override fun onPause() {
        runCatching { unregisterReceiver(receiver) }
        super.onPause()
    }

    private fun render() {
        val state = ProxyService.state
        val mode = Prefs.netMode(this)
        val waiting = state == ProxyService.STATE_WAITING
        val connected = state == ProxyService.STATE_CONNECTED
        val conns = ProxyService.activeConnections.get()

        statusTitle.text = when {
            connected -> getString(R.string.status_connected_title)
            waiting -> getString(R.string.status_waiting_title)
            else -> getString(R.string.status_idle_title)
        }

        val modeLabel = getString(
            if (mode == Prefs.NET_MODE_CELLULAR) R.string.mode_cellular else R.string.mode_default
        )
        btnNetMode.text = getString(R.string.action_net_mode, modeLabel)

        statusDetail.text = when {
            connected && !ProxyService.netReady -> getString(R.string.status_no_exit_net, modeLabel)
            connected -> getString(R.string.status_connected_detail, modeLabel, conns)
            else -> getString(R.string.status_idle_detail)
        }

        val dot = when {
            waiting -> R.color.status_waiting
            connected && !ProxyService.netReady -> R.color.status_failed
            connected -> R.color.status_connected
            else -> R.color.status_idle
        }
        statusDot.imageTintList = ContextCompat.getColorStateList(this, dot)

        steps.visibility = if (waiting) View.VISIBLE else View.GONE
        statusDetail.visibility = if (waiting) View.GONE else View.VISIBLE

        if (waiting || connected) {
            btnPrimary.visibility = View.GONE
            btnStop.visibility = View.VISIBLE
            btnStop.text = getString(if (connected) R.string.action_stop else R.string.action_cancel)
        } else {
            btnStop.visibility = View.GONE
            btnPrimary.visibility = View.VISIBLE
            btnPrimary.text = getString(R.string.action_start)
        }

        if (connected) {
            window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        } else {
            window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        }
    }
}

fun AppCompatActivity.applyLightSystemBars() {
    WindowCompat.getInsetsController(window, window.decorView).apply {
        isAppearanceLightStatusBars = true
        isAppearanceLightNavigationBars = true
    }
}
