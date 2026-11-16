package io.github.kidoflamancha.usbrelay

import android.content.Context

object Prefs {
    private const val NAME = "usbrelay"
    private const val KEY_NET_MODE = "net_mode"

    const val NET_MODE_DEFAULT = 0
    const val NET_MODE_CELLULAR = 1

    fun netMode(ctx: Context): Int {
        return ctx.getSharedPreferences(NAME, Context.MODE_PRIVATE)
            .getInt(KEY_NET_MODE, NET_MODE_DEFAULT)
    }

    fun setNetMode(ctx: Context, mode: Int) {
        ctx.getSharedPreferences(NAME, Context.MODE_PRIVATE)
            .edit()
            .putInt(KEY_NET_MODE, mode)
            .apply()
    }
}
