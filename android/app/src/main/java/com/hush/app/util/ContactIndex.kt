package com.hush.app.util

import android.icu.text.AlphabeticIndex
import android.icu.text.Collator
import android.icu.util.ULocale

/**
 * 通讯录分组与排序：中文名按拼音首字母归入 A–Z（ICU 中文拼音排序规则），英文按首字母，其余归入“#”。
 * 例如「测试助手」归 C、「小二」归 X。
 */
object ContactIndex {
    private val index by lazy {
        AlphabeticIndex<Any>(ULocale.SIMPLIFIED_CHINESE).addLabels(ULocale.ENGLISH).buildImmutableIndex()
    }
    private val collator: Collator by lazy { Collator.getInstance(ULocale.SIMPLIFIED_CHINESE) }

    fun bucketOf(name: String): String {
        val trimmed = name.trim()
        if (trimmed.isEmpty()) return "#"
        val label = runCatching { index.getBucket(index.getBucketIndex(trimmed)).label }.getOrNull() ?: return "#"
        return if (label.length == 1 && label[0] in 'A'..'Z') label else "#"
    }

    /** 按首字母分组（A–Z 在前，“#”在最后），组内按拼音/字母顺序。 */
    fun <T> group(items: List<T>, name: (T) -> String): List<Pair<String, List<T>>> =
        items.groupBy { bucketOf(name(it)) }
            .toSortedMap(compareBy<String> { it == "#" }.thenBy { it })
            .map { (k, v) -> k to v.sortedWith { a, b -> collator.compare(name(a), name(b)) } }
}
