import SwiftUI
import UIKit

extension AttachmentItem {
    /// 本地已有的明文文件路径（存在才返回）
    var existingPath: String? {
        guard let p = localPath, FileManager.default.fileExists(atPath: p) else { return nil }
        return p
    }
}

/// 下载附件（结果会通过 MessageUpdated 事件回写 localPath）
@MainActor
enum AttachmentLoader {
    static func ensure(_ m: MessageItem, index: Int) async -> String? {
        guard case let .attachments(items, _) = m.body, index < items.count else { return nil }
        if let p = items[index].existingPath { return p }
        do {
            return try await HushCore.shared.chats.downloadAttachment(m.id, index: index)
        } catch {
            HushCore.shared.toast = "下载失败：\(describe(error))"
            return nil
        }
    }
}

/// 图片消息：先显示缩略图（模糊），自动下载后显示原图，点击全屏
struct ImageAttachmentView: View {
    let message: MessageItem
    let item: AttachmentItem
    let index: Int
    var onOpen: (String) -> Void

    private var displaySize: CGSize {
        let maxW: CGFloat = 220, maxH: CGFloat = 280
        let w = CGFloat(max(item.width, 1)), h = CGFloat(max(item.height, 1))
        guard item.width > 0, item.height > 0 else { return CGSize(width: 200, height: 200) }
        let s = min(maxW / w, maxH / h)
        return CGSize(width: max(w * s, 100), height: max(h * s, 80))
    }

    @ViewBuilder private var placeholder: some View {
        if let t = item.thumbnail, let img = UIImage(data: t) {
            ZStack {
                Image(uiImage: img).resizable().scaledToFill().blur(radius: 8)
                ProgressView().tint(.white)
            }
        } else {
            ZStack {
                Theme.surface
                ProgressView()
            }
        }
    }

    var body: some View {
        ZStack {
            if let p = item.existingPath {
                LocalImage(path: p, maxPixel: 700) { placeholder }
            } else {
                placeholder
            }
        }
        .frame(width: displaySize.width, height: displaySize.height)
        .clipShape(RoundedRectangle(cornerRadius: 14))
        .contentShape(Rectangle())
        .onTapGesture {
            if let p = item.existingPath { onOpen(p) } else {
                Task { if let p = await AttachmentLoader.ensure(message, index: index) { onOpen(p) } }
            }
        }
        .task(id: message.id) {
            if item.existingPath == nil && !message.deleted { _ = await AttachmentLoader.ensure(message, index: index) }
        }
        .accessibilityElement()
        .accessibilityLabel("图片")
        .accessibilityIdentifier("imageMessage")
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

    var body: some View {
        let playing = player.playingId == message.id
        let fg = outgoing ? Theme.onAccent : Theme.textPrimary
        HStack(spacing: 10) {
            Image(systemName: playing ? "stop.circle.fill" : "play.circle.fill")
                .font(.system(size: 30))
                .foregroundStyle(fg)
            VStack(alignment: .leading, spacing: 4) {
                Waveform(seed: message.id, progress: playing ? player.progress : 0, color: fg)
                    .frame(width: waveWidth, height: 20)
                Text(Fmt.duration(Int((item.durationMs + 500) / 1000)))
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
        .accessibilityLabel("语音 \(Int(item.durationMs / 1000)) 秒")
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
    @State private var loading = false

    var body: some View {
        let fg = outgoing ? Theme.onAccent : Theme.textPrimary
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
                Text(Fmt.fileSize(item.size)).font(Fonts.timestamp).foregroundStyle(fg.opacity(0.75))
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
