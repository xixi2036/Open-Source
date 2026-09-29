package com.hush.app.ui.chat

/** 聊天分页的纯逻辑（JVM 单测见 ChatPagingTest）。 */
object ChatPaging {
    /** 距列表顶部（reverseLayout 下为最大下标）还剩几项时开始加载更早消息。 */
    const val MARGIN = 3

    /**
     * 是否已滚到接近最早一条：必须用同一次布局的可见项与总项数判断。
     * （旧实现把第一次组合时的空列表长度闭包进 derivedStateOf，阈值变成 -3，打开会话就把整段历史读完。）
     */
    fun nearTop(lastVisibleIndex: Int?, totalItems: Int, margin: Int = MARGIN): Boolean =
        lastVisibleIndex != null && totalItems > 0 && lastVisibleIndex >= totalItems - margin

    /**
     * 重连后用最新一页校正已加载列表（事件总线溢出或断线期间可能漏掉增删改）。
     * current / page 均按时间升序。page 的第一条在 current 中时，用 page 替换该位置之后的尾部；
     * 否则说明漏掉的比一页还多，返回 null，由调用方整体换成 page。
     */
    fun <T> mergeLatest(current: List<T>, page: List<T>, id: (T) -> String): List<T>? {
        if (page.isEmpty()) return current
        val anchor = id(page.first())
        val idx = current.indexOfFirst { id(it) == anchor }
        if (idx < 0) return if (current.isEmpty()) page else null
        return current.subList(0, idx) + page
    }
}
