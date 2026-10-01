import SwiftUI
import UIKit

/// 聊天输入框：包装 UITextView，1–5 行自动增高。
/// 用 UIKit 而不是 SwiftUI TextField：输入框文本被程序改写（插入 @成员、整段删除提及）后要把光标放回
/// 插入片段之后 / 删除处。TextField 在 iOS 17 拿不到也设不了光标；iOS 18 的 `TextField(text:selection:)`
/// 在「编辑回调里改写文本」时实测不生效（UITextView 保留旧偏移，写入的 TextSelection 被忽略）。
/// UITextView 的 selectedRange 以 UTF-16 计，与 MentionComposer 的偏移单位一致。
struct MessageTextView: UIViewRepresentable {
    @Binding var text: String
    /// 程序改写文本后应放置的光标（UTF-16）；应用后置 nil
    @Binding var pendingCursor: Int?
    /// 第一次获得焦点时把光标放到文本末尾（恢复草稿后使用，C1）；生效后置 false
    var cursorToEndOnFocus: Binding<Bool> = .constant(false)
    var placeholder: String
    /// A5：跟随系统字号（动态字体）
    var font: UIFont = UIFontMetrics(forTextStyle: .callout).scaledFont(for: .systemFont(ofSize: 16))
    var maxLines: Int = 5

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeUIView(context: Context) -> UITextView {
        let v = UITextView()
        v.delegate = context.coordinator
        v.font = font
        v.adjustsFontForContentSizeCategory = true
        v.textColor = UIColor(Theme.textPrimary)
        v.tintColor = UIColor(Theme.accent)
        v.backgroundColor = .clear
        v.textContainerInset = .zero
        v.textContainer.lineFragmentPadding = 0
        v.isScrollEnabled = true
        v.showsVerticalScrollIndicator = false
        v.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        v.accessibilityIdentifier = "messageInput"
        v.accessibilityLabel = placeholder
        v.text = text
        return v
    }

    func updateUIView(_ v: UITextView, context: Context) {
        context.coordinator.parent = self
        // 只有程序改写（与输入框内容不同）时才写回；输入法组字（marked text）期间文本由用户输入同步而来，两者相等
        if v.text != text {
            v.text = text
        }
        if let pos = pendingCursor {
            let len = (v.text as NSString).length
            var loc = min(max(pos, 0), len)
            // 不落在代理对中间
            if loc > 0 && loc < len, UTF16.isTrailSurrogate((v.text as NSString).character(at: loc)) { loc -= 1 }
            v.selectedRange = NSRange(location: loc, length: 0)
            v.scrollRangeToVisible(v.selectedRange)
            DispatchQueue.main.async { pendingCursor = nil }
        }
    }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView v: UITextView, context: Context) -> CGSize? {
        let width = proposal.width ?? 240
        let line = ceil((v.font ?? font).lineHeight)
        let fit = v.sizeThatFits(CGSize(width: width, height: .greatestFiniteMagnitude)).height
        return CGSize(width: width, height: min(max(ceil(fit), line), line * CGFloat(maxLines)))
    }

    final class Coordinator: NSObject, UITextViewDelegate {
        var parent: MessageTextView
        init(_ parent: MessageTextView) { self.parent = parent }

        func textViewDidChange(_ v: UITextView) {
            if parent.text != v.text { parent.text = v.text }
        }

        func textViewDidBeginEditing(_ v: UITextView) {
            guard parent.cursorToEndOnFocus.wrappedValue else { return }
            // 点击输入框时系统会把光标放到点击处：等这次点击处理完再移到末尾
            DispatchQueue.main.async { [weak v, parent] in
                guard let v else { return }
                let end = (v.text as NSString).length
                v.selectedRange = NSRange(location: end, length: 0)
                v.scrollRangeToVisible(v.selectedRange)
                parent.cursorToEndOnFocus.wrappedValue = false
            }
        }
    }
}
