package com.hush.app.util

import com.hush.app.core.ContactsStore
import uniffi.hushcore.AttachmentKind
import uniffi.hushcore.CallEndReason
import uniffi.hushcore.MessageBody
import uniffi.hushcore.MessageItem

/** 消息的文本摘要（会话列表预览、通知、引用）。 */
object MessageText {
    /**
     * [nameOf] 取发送者名字。默认的 ContactsStore.nameOf 在缺资料时会发起网络请求；
     * 组合中调用时应传入已订阅的名字（见 ConversationRow），避免组合期副作用。
     */
    fun preview(m: MessageItem, isGroup: Boolean = false, nameOf: (String) -> String = ContactsStore::nameOf): String {
        if (m.deleted) {
            return if (m.isOutgoing) "你撤回了一条消息" else "\"${nameOf(m.senderId)}\" 撤回了一条消息"
        }
        val body = when (val b = m.body) {
            is MessageBody.Text -> b.text
            is MessageBody.Attachments -> attachmentsSummary(b)
            is MessageBody.System -> return b.text
            is MessageBody.Undecryptable -> "⚠️ 无法解密的消息"
            is MessageBody.CallLog -> callLogPreview(b.video, b.durationSecs.toLong(), b.reason)
        }
        return if (isGroup && !m.isOutgoing && m.senderId.isNotEmpty() && m.body !is MessageBody.CallLog) {
            "${nameOf(m.senderId)}: $body"
        } else body
    }

    fun attachmentsSummary(b: MessageBody.Attachments): String {
        val first = b.items.firstOrNull() ?: return b.caption ?: ""
        return when (first.kind) {
            AttachmentKind.IMAGE -> "[图片]"
            AttachmentKind.VOICE -> "[语音]"
            AttachmentKind.VIDEO -> "[视频]"
            AttachmentKind.FILE -> "[文件] ${first.fileName}"
        }
    }

    /** 聊天页通话记录（旁边有电话/摄像头图标）：按核心给出的结束原因显示（P-B，与 iOS 一致）。 */
    fun callLog(b: MessageBody.CallLog): String = callLogText(b.video, b.durationSecs.toLong(), b.reason)

    fun callLogText(video: Boolean, secs: Long, reason: CallEndReason): String = when (reason) {
        CallEndReason.COMPLETED -> "${if (video) "视频通话" else "语音通话"} ${TimeFormat.duration(secs)}"
        CallEndReason.MISSED -> "未接来电"
        CallEndReason.REJECTED -> "已拒绝"
        CallEndReason.BUSY -> "对方忙线"
        CallEndReason.CANCELLED -> "已取消"
        CallEndReason.FAILED -> "通话失败"
    }

    /** 会话列表/搜索的单行预览没有图标：“已拒绝”“已取消”这类短语前补上通话类型。 */
    fun callLogPreview(video: Boolean, secs: Long, reason: CallEndReason): String {
        val text = callLogText(video, secs, reason)
        return when (reason) {
            CallEndReason.COMPLETED, CallEndReason.MISSED -> "[$text]"
            else -> "[${if (video) "视频通话" else "语音通话"} $text]"
        }
    }
}
