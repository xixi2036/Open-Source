import SwiftUI
import PhotosUI
import AVFoundation
import UniformTypeIdentifiers

/// 准备好待发送的一项媒体
enum PreparedMedia: Sendable {
    case image(url: URL, width: Int, height: Int, thumbnail: Data?)
    case video(PreparedVideo)
}

struct PreparedVideo: Sendable {
    var url: URL
    var mime: String
    var width: Int
    var height: Int
    var durationMs: UInt64
    var thumbnail: Data?
    var size: UInt64
}

enum MediaLoadError: Error, Equatable {
    case unreadable
    /// 视频超过大小上限
    case tooLarge(UInt64)
}

/// 相册里的视频：以文件形式导入（不把整个视频读进内存），拷贝到 outbox 的独立子目录
struct PickedVideo: Transferable {
    let url: URL

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(importedContentType: .movie) { received in
            let dir = AppPaths.outbox.appendingPathComponent(UUID().uuidString, isDirectory: true)
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let ext = received.file.pathExtension.isEmpty ? "mov" : received.file.pathExtension.lowercased()
            let dst = dir.appendingPathComponent("video.\(ext)")
            try FileManager.default.copyItem(at: received.file, to: dst)
            return PickedVideo(url: dst)
        }
    }
}

enum PhotoLoader {
    /// 视频大小上限（与核心的附件上限一致）
    static let maxVideoBytes: UInt64 = 100 * 1024 * 1024

    /// 读取相册选择项并压缩为 JPEG（长边 ≤ maxSide）。解码与压缩在后台线程（C4）。
    static func load(_ item: PhotosPickerItem, maxSide: CGFloat = 2048) async -> (url: URL, width: Int, height: Int)? {
        guard let data = try? await item.loadTransferable(type: Data.self) else { return nil }
        return await Task.detached(priority: .userInitiated) { () -> (url: URL, width: Int, height: Int)? in
            guard let r = ImageUtil.compressToJPEG(data, maxSide: maxSide) else { return nil }
            return (r.0, r.1, r.2)
        }.value
    }

    static func isVideo(_ item: PhotosPickerItem) -> Bool {
        item.supportedContentTypes.contains { $0.conforms(to: .movie) || $0.conforms(to: .video) }
    }

    /// 聊天里发送用：图片压缩并生成缩略图；视频拷贝出来、取时长 / 尺寸 / 封面，超过上限报错
    static func prepare(_ item: PhotosPickerItem) async -> Result<PreparedMedia, MediaLoadError> {
        if isVideo(item) {
            guard let picked = try? await item.loadTransferable(type: PickedVideo.self) else { return .failure(.unreadable) }
            return await prepareVideo(at: picked.url).map { .video($0) }
        }
        guard let data = try? await item.loadTransferable(type: Data.self) else { return .failure(.unreadable) }
        let r = await Task.detached(priority: .userInitiated) { () -> PreparedMedia? in
            guard let c = ImageUtil.compressToJPEG(data) else { return nil }
            return .image(url: c.0, width: c.1, height: c.2, thumbnail: ImageUtil.thumbnail(c.0))
        }.value
        return r.map { .success($0) } ?? .failure(.unreadable)
    }

    /// 拍照得到的图片文件
    static func prepareImage(fileURL: URL) async -> PreparedMedia? {
        await Task.detached(priority: .userInitiated) { () -> PreparedMedia? in
            guard let c = ImageUtil.compressToJPEG(fileURL: fileURL) else { return nil }
            return .image(url: c.0, width: c.1, height: c.2, thumbnail: ImageUtil.thumbnail(c.0))
        }.value
    }

    /// 已在 outbox 里的视频文件 → 元数据与封面；超过上限时删除文件并返回 tooLarge
    static func prepareVideo(at url: URL, maxBytes: UInt64 = maxVideoBytes) async -> Result<PreparedVideo, MediaLoadError> {
        let size = ((try? FileManager.default.attributesOfItem(atPath: url.path))?[.size] as? NSNumber)?.uint64Value ?? 0
        if let error = VideoMeta.sizeError(size, max: maxBytes) {
            AppPaths.discardOutbox(url.path)
            return .failure(error)
        }
        let asset = AVURLAsset(url: url)
        var width = 0, height = 0
        var durationMs: UInt64 = 0
        if let d = try? await asset.load(.duration), d.isNumeric { durationMs = UInt64(max(0, CMTimeGetSeconds(d)) * 1000) }
        if let track = try? await asset.loadTracks(withMediaType: .video).first,
           let natural = try? await track.load(.naturalSize), let transform = try? await track.load(.preferredTransform) {
            let shown = VideoMeta.displaySize(natural: natural, transform: transform)
            width = Int(shown.width); height = Int(shown.height)
        }
        let thumb = await VideoThumbnails.generate(url, maxPixel: 240)
        return .success(PreparedVideo(url: url, mime: MimeType.of(url), width: width, height: height,
                                      durationMs: durationMs, thumbnail: thumb?.jpegData(compressionQuality: 0.5), size: size))
    }
}

/// 视频元数据的纯逻辑
enum VideoMeta {
    static func sizeError(_ size: UInt64, max: UInt64) -> MediaLoadError? {
        if size == 0 { return .unreadable }
        return size > max ? .tooLarge(size) : nil
    }

    /// 应用旋转后的显示尺寸（竖拍视频的 naturalSize 是横的）
    static func displaySize(natural: CGSize, transform: CGAffineTransform) -> CGSize {
        let r = CGRect(origin: .zero, size: natural).applying(transform)
        return CGSize(width: abs(r.width).rounded(), height: abs(r.height).rounded())
    }

    static func tooLargeText(_ size: UInt64, max: UInt64 = PhotoLoader.maxVideoBytes) -> String {
        "视频 \(Fmt.fileSize(size))，超过 \(max / 1024 / 1024)MB 上限，无法发送"
    }
}

/// 本地视频的封面帧（已下载的视频在气泡里显示第一帧）。内存缓存，退出登录时清空。
enum VideoThumbnails {
    nonisolated(unsafe) private static let cache: NSCache<NSString, UIImage> = {
        let c = NSCache<NSString, UIImage>()
        c.countLimit = 60
        return c
    }()

    static func cached(_ path: String) -> UIImage? { cache.object(forKey: path as NSString) }

    static func load(_ path: String) async -> UIImage? {
        if let img = cached(path) { return img }
        guard let img = await generate(URL(fileURLWithPath: path), maxPixel: 600) else { return nil }
        cache.setObject(img, forKey: path as NSString)
        return img
    }

    static func generate(_ url: URL, maxPixel: CGFloat) async -> UIImage? {
        let gen = AVAssetImageGenerator(asset: AVURLAsset(url: url))
        gen.appliesPreferredTrackTransform = true
        gen.maximumSize = CGSize(width: maxPixel, height: maxPixel)
        guard let cg = try? await gen.image(at: .zero).image else { return nil }
        return UIImage(cgImage: cg)
    }

    static func removeAll() { cache.removeAllObjects() }
}
