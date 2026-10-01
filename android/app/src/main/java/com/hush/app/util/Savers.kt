package com.hush.app.util

import android.net.Uri
import androidx.compose.runtime.saveable.Saver
import androidx.compose.runtime.saveable.listSaver

/**
 * rememberSaveable 用的 Saver（A4）：界面上“用户已经输入/选择但还没提交”的内容要能跨 Activity 重建
 * （改系统字体、切换深色模式、进程被回收后恢复）保留。只保存 id、文字、URI 字符串，不保存核心对象。
 */
object Savers {
    val StringSet: Saver<Set<String>, Any> = listSaver(save = { it.toList() }, restore = { it.toSet() })

    /** 保持选择顺序的集合（转发目标按选择顺序发送）。 */
    val OrderedStringSet: Saver<LinkedHashSet<String>, Any> = listSaver(save = { it.toList() }, restore = { LinkedHashSet(it) })

    val StringList: Saver<List<String>, Any> = listSaver(save = { it }, restore = { it })

    val UriList: Saver<List<Uri>, Any> = listSaver(save = { list -> list.map { it.toString() } }, restore = { list -> list.map(Uri::parse) })

    /** (id, 可空的第二个 id)，例如正在评论的 (动态 id, 被回复的评论 id)。 */
    val IdPair: Saver<Pair<String, String?>?, Any> = listSaver(
        save = { p -> if (p == null) emptyList() else listOf(p.first, p.second ?: "") },
        restore = { l -> if (l.isEmpty()) null else l[0] to l[1].ifEmpty { null } },
    )
}
