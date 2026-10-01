import SwiftUI
import UIKit

/// C7：附件文件是否存在的短时缓存。气泡每次渲染都要知道「本地有没有这个文件」，
/// 直接 fileExists 会在滚动时反复做系统调用；这里把结果记 10 秒。
/// 用户点击（打开、播放、分享）时用 fresh: true 重新检查，避免用到已被核心删除的文件。
enum AttachmentFiles {
    private struct Entry { var exists: Bool; var at: CFAbsoluteTime }
    nonisolated(unsafe) private static var entries: [String: Entry] = [:]
    private static let lock = NSLock()
    static let ttl: CFAbsoluteTime = 10

    static func exists(_ path: String, fresh: Bool = false, now: CFAbsoluteTime = CFAbsoluteTimeGetCurrent()) -> Bool {
        lock.lock(); defer { lock.unlock() }
        if !fresh, let e = entries[path], now - e.at < ttl { return e.exists }
        let ok = FileManager.default.fileExists(atPath: path)
        if entries.count > 2000 { entries.removeAll() }
        entries[path] = Entry(exists: ok, at: now)
        return ok
    }

    static func reset() {
        lock.lock(); defer { lock.unlock() }
        entries.removeAll()
    }
}

extension AttachmentItem {
    /// 本地已有的明文文件路径（存在才返回；结果有短时缓存）
    var existingPath: String? {
        guard let p = localPath, AttachmentFiles.exists(p) else { return nil }
        return p
    }

    /// 同上，但重新检查文件是否还在（用户点击时用）
    var verifiedPath: String? {
        guard let p = localPath, AttachmentFiles.exists(p, fresh: true) else { return nil }
        return p
    }
}

/// 下载附件（结果会通过 MessageUpdated 事件回写 localPath）
@MainActor
enum AttachmentLoader {
    /// userInitiated：用户点击触发——失败时提示一次原因，并且会重试之前失败过的附件。
    /// 自动下载（滚入屏幕）失败时不弹提示，只记录失败状态，之后也不再自动重试（B5）。
    static func ensure(_ m: MessageItem, index: Int, userInitiated: Bool = true) async -> String? {
        guard case let .attachments(items, _) = m.body, index < items.count else { return nil }
        if let p = items[index].verifiedPath { return p }
        let chats = HushCore.shared.chats
        if !userInitiated, chats.downloadFailure(m.id, index) != nil { return nil }
        do {
            let p = try await chats.downloadAttachment(m.id, index: index)
            if let p { _ = AttachmentFiles.exists(p, fresh: true) }
            return p
        } catch {
            if userInitiated {
                let f = DownloadFailure.classify(error)
                HushCore.shared.toast = f == .expired ? "附件已过期，无法下载" : "下载失败：\(describe(error))"
            }
            return nil
        }
    }
}

/// 附件下载失败 / 已过期时盖在缩略图上的状态
struct AttachmentFailureBadge: View {
    let failure: DownloadFailure
    var body: some View {
        VStack(spacing: 6) {
            Image(systemName: failure == .expired ? "clock.badge.xmark" : "arrow.clockwise.circle.fill")
                .font(.system(size: 26))
            Text(failure.text).font(Fonts.timestamp).multilineTextAlignment(.center)
        }
        .foregroundStyle(.white)
        .padding(10)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color.black.opacity(0.45))
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier(failure == .expired ? "attachmentExpired" : "attachmentFailed")
    }
}

/// 图片 / 视频气泡的显示尺寸
func mediaDisplaySize(width: UInt32, height: UInt32) -> CGSize {
    let maxW: CGFloat = 220, maxH: CGFloat = 280
    guard width > 0, height > 0 else { return CGSize(width: 200, height: 200) }
    let w = CGFloat(width), h = CGFloat(height)
    let s = min(maxW / w, maxH / h)
    return CGSize(width: max(w * s, 100), height: max(h * s, 80))
}

/// 图片消息：先显示缩略图（模糊），自动下载后显示原图，点击全屏。
/// 自动下载失败后显示失败态（点击重试）；服务器上已清理的显示「附件已过期」。
struct ImageAttachmentView: View {
    let message: MessageItem
    let item: AttachmentItem
    let index: Int
    var onOpen: (String) -> Void
    var chats = HushCore.shared.chats

    private var displaySize: CGSize { mediaDisplaySize(width: item.width, height: item.height) }
    private var failure: DownloadFailure? { chats.downloadFailure(message.id, index) }

    @ViewBuilder private var placeholder: some View {
        if let t = item.thumbnail, let img = UIImage(data: t) {
            ZStack {
                Image(uiImage: img).resizable().scaledToFill().blur(radius: 8)
                if failure == nil { ProgressView().tint(.white) }
            }
        } else {
            ZStack {
                Theme.surface
                if failure == nil { ProgressView() }
            }
        }
    }

    var body: some View {
        let local = item.existingPath
        ZStack {
            if let p = local {
                LocalImage(path: p, maxPixel: 700) { placeholder }
            } else {
                placeholder
                if let failure { AttachmentFailureBadge(failure: failure) }
            }
        }
        .frame(width: displaySize.width, height: displaySize.height)
        .clipShape(RoundedRectangle(cornerRadius: 14))
        .contentShape(Rectangle())
        .onTapGesture {
            Task { if let p = await AttachmentLoader.ensure(message, index: index) { onOpen(p) } }
        }
        .task(id: message.id) {
            if local == nil && !message.deleted { _ = await AttachmentLoader.ensure(message, index: index, userInitiated: false) }
        }
        .accessibilityElement()
        .accessibilityLabel(local == nil ? (failure?.text ?? "图片，正在下载") : "图片")
        .accessibilityIdentifier("imageMessage")
        .accessibilityAddTraits(.isButton)
    }
}

/// 视频消息：封面（发送方附带的缩略图，或已下载文件的首帧）+ 播放按钮 + 时长 / 大小。
/// 不自动下载（可能很大）；点击后下载并用内置播放器播放。
struct VideoAttachmentView: View {
    let message: MessageItem
    let item: AttachmentItem
    let index: Int
    var onOpen: (String) -> Void
    var chats = HushCore.shared.chats
    @State private var loading = false
    @State private var cover: UIImage?

    private var displaySize: CGSize { mediaDisplaySize(width: item.width, height: item.height) }
    private var failure: DownloadFailure? { chats.downloadFailure(message.id, index) }

    var body: some View {
        let local = item.existingPath
        ZStack {
            if let cover {
                Image(uiImage: cover).resizable().scaledToFill()
            } else if let t = item.thumbnail, let img = UIImage(data: t) {
                Image(uiImage: img).resizable().scaledToFill()
            } else {
                Color.black.opacity(0.75)
            }
            if let failure, local == nil {
                AttachmentFailureBadge(failure: failure)
            } else if loading {
                ProgressView().tint(.white).controlSize(.large)
            } else {
                Image(systemName: local == nil ? "arrow.down.circle.fill" : "play.circle.fill")
                    .font(.system(size: 46)).foregroundStyle(.white.opacity(0.92))
                    .shadow(color: .black.opacity(0.4), radius: 4)
            }
        }
        .frame(width: displaySize.width, height: displaySize.height)
        .overlay(alignment: .bottomLeading) {
            HStack(spacing: 4) {
                Image(systemName: "video.fill")
                Text(item.durationMs > 0 ? Fmt.duration(Int((item.durationMs + 500) / 1000)) : "视频")
                if local == nil { Text("· " + Fmt.fileSize(item.size)) }
            }
            .font(Fonts.timestamp).foregroundStyle(.white)
            .padding(.horizontal, 6).padding(.vertical, 3)
            .background(Capsule().fill(.black.opacity(0.5)))
            .padding(6)
        }
        .clipShape(RoundedRectangle(cornerRadius: 14))
        .contentShape(Rectangle())
        .onTapGesture {
            guard !loading else { return }
            loading = true
            Task {
                if let p = await AttachmentLoader.ensure(message, index: index) { onOpen(p) }
                loading = false
            }
        }
        .task(id: local) {
            if let local { cover = await VideoThumbnails.load(local) }
        }
        .accessibilityElement()
        .accessibilityLabel("视频 \(Int(item.durationMs / 1000)) 秒" + (failure.map { "，" + $0.text } ?? ""))
        .accessibilityIdentifier("videoMessage")
        .accessibilityAddTraits(.isButton)
    }
}

/// 语音消息：播放按钮 + 进度条 + 时长
struct VoiceAttachmentView: View {
    let message: MessageItem
    let item: AttachmentItem
    let index: Int
    let outgoing: Bool
    var player = VoicePlayer.shared
    var chats = HushCore.shared.chats

    var body: some View {
        let playing = player.playingId == message.id
        let fg = outgoing ? Theme.onAccent : Theme.textPrimary
        let failure = item.existingPath == nil ? chats.downloadFailure(message.id, index) : nil
        HStack(spacing: 10) {
            Image(systemName: playing ? "stop.circle.fill" : "play.circle.fill")
                .font(.system(size: 30))
                .foregroundStyle(fg)
            VStack(alignment: .leading, spacing: 4) {
                Waveform(seed: message.id, progress: playing ? player.progress : 0, color: fg)
                    .frame(width: waveWidth, height: 20)
                HStack(spacing: 6) {
                    Text(Fmt.duration(Int((item.durationMs + 500) / 1000)))
                    if playing && player.earpiece { Image(systemName: "ear").accessibilityLabel("听筒播放") }
                    if let failure { Text(failure.text).accessibilityIdentifier("attachmentFailedText") }
                }
                .font(Fonts.timestamp).foregroundStyle(fg.opacity(0.8))
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle())
        .onTapGesture {
            Task {
                if let p = await AttachmentLoader.ensure(message, index: index) { player.toggle(id: message.id, path: p) }
            }
        }
        .accessibilityElement()
        .accessibilityLabel("语音 \(Int(item.durationMs / 1000)) 秒" + (playing ? "，正在播放" : ""))
        .accessibilityIdentifier("voiceMessage")
        .accessibilityAddTraits(.isButton)
    }

    private var waveWidth: CGFloat {
        let secs = CGFloat(item.durationMs) / 1000
        return min(150, 70 + secs * 4)
    }
}

/// 伪波形（按消息 id 生成稳定形状）+ 播放进度
struct Waveform: View {
    let seed: String
    let progress: Double
    let color: Color

    var body: some View {
        GeometryReader { geo in
            let count = max(Int(geo.size.width / 4), 8)
            let heights = Self.heights(seed: seed, count: count)
            HStack(alignment: .center, spacing: 2) {
                ForEach(0..<count, id: \.self) { i in
                    Capsule()
                        .fill(color.opacity(Double(i) / Double(count) <= progress && progress > 0 ? 1 : 0.45))
                        .frame(width: 2, height: geo.size.height * heights[i])
                }
            }
            .frame(maxHeight: .infinity)
        }
    }

    static func heights(seed: String, count: Int) -> [CGFloat] {
        var h: UInt32 = 2166136261
        for b in seed.utf8 { h = (h ^ UInt32(b)) &* 16777619 }
        return (0..<count).map { _ in
            h = h &* 1103515245 &+ 12345
            return 0.25 + CGFloat((h >> 16) % 75) / 100
        }
    }
}

/// 文件消息：图标 + 名称 + 大小；点击下载后分享/打开
struct FileAttachmentView: View {
    let message: MessageItem
    let item: AttachmentItem
    let index: Int
    let outgoing: Bool
    var onShare: (URL) -> Void
    var chats = HushCore.shared.chats
    @State private var loading = false

    var body: some View {
        let fg = outgoing ? Theme.onAccent : Theme.textPrimary
        let failure = item.existingPath == nil ? chats.downloadFailure(message.id, index) : nil
        HStack(spacing: 10) {
            ZStack {
                RoundedRectangle(cornerRadius: 8).fill(outgoing ? Color.white.opacity(0.2) : Theme.accent.opacity(0.15))
                if loading { ProgressView() } else {
                    Image(systemName: "doc.fill").foregroundStyle(outgoing ? Theme.onAccent : Theme.accent)
                }
            }
            .frame(width: 40, height: 44)
            VStack(alignment: .leading, spacing: 2) {
                Text(item.fileName).font(Fonts.body).foregroundStyle(fg).lineLimit(2)
                Text(failure?.text ?? Fmt.fileSize(item.size)).font(Fonts.timestamp).foregroundStyle(fg.opacity(0.75))
            }
        }
        .frame(maxWidth: 240, alignment: .leading)
        .contentShape(Rectangle())
        .onTapGesture {
            loading = true
            Task {
                if let p = await AttachmentLoader.ensure(message, index: index) { onShare(Self.shareCopy(p, name: item.fileName)) }
                loading = false
            }
        }
        .accessibilityElement()
        .accessibilityLabel("文件 \(item.fileName)")
        .accessibilityIdentifier("fileMessage")
        .accessibilityAddTraits(.isButton)
    }
}

extension FileAttachmentView {
    /// 以原文件名复制一份临时文件再分享（核心缓存文件名带消息 id 前缀）。
    /// 文件名来自对方，必须清洗；每次分享放进新的 <UUID> 子目录，绝不按远端数据拼出的路径做删除。
    static func shareCopy(_ path: String, name: String, base: URL = AppPaths.shareDir) -> URL {
        let src = URL(fileURLWithPath: path)
        clearShareCopies(base: base)
        let dir = base.appendingPathComponent(UUID().uuidString, isDirectory: true)
        do { try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true) } catch { return src }
        let dst = dir.appendingPathComponent(safeFileName(name, fallback: src.lastPathComponent))
        guard dst.deletingLastPathComponent().standardizedFileURL == dir.standardizedFileURL else { return src }
        do { try FileManager.default.copyItem(at: src, to: dst) } catch { return src }
        return dst
    }

    /// 删除之前的分享副本（整个 share 目录，路径与远端数据无关）
    static func clearShareCopies(base: URL = AppPaths.shareDir) {
        try? FileManager.default.removeItem(at: base)
    }

    /// 去掉路径分隔符与控制字符；"."、".." 或空名回退为 fallback
    static func safeFileName(_ name: String, fallback: String) -> String {
        var n = String(name.map { $0 == "/" || $0 == ":" || $0 == "\\" ? "_" : $0 })
        n = String(n.unicodeScalars.filter { !CharacterSet.controlCharacters.contains($0) }.map(Character.init))
        n = n.trimmingCharacters(in: .whitespacesAndNewlines)
        if n.count > 180 { n = String(n.suffix(180)) }
        if n.isEmpty || n.allSatisfy({ $0 == "." }) { return fallback }
        return n
    }
}

/// 系统分享面板
struct ShareSheet: UIViewControllerRepresentable {
    let items: [Any]
    func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: items, applicationActivities: nil)
    }
    func updateUIViewController(_ vc: UIActivityViewController, context: Context) {}
}

struct IdentifiableURL: Identifiable {
    let url: URL
    var id: String { url.path }
}
