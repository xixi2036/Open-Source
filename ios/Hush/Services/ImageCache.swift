import UIKit
import ImageIO
import SwiftUI

/// 本地图片解码缓存（按路径 + 最大边长降采样）。解码可以在任意线程进行（NSCache 线程安全）。
enum ImageCache {
    nonisolated(unsafe) private static let cache: NSCache<NSString, UIImage> = {
        let c = NSCache<NSString, UIImage>()
        c.countLimit = 300
        c.totalCostLimit = 64 * 1024 * 1024   // 按解码后的字节数计
        return c
    }()

    private static func key(_ path: String, _ maxPixel: CGFloat) -> NSString { "\(path)#\(Int(maxPixel))" as NSString }

    /// 只查内存缓存，不做任何 IO（可在 View body 中调用）
    static func cached(_ path: String, maxPixel: CGFloat = 600) -> UIImage? {
        cache.object(forKey: key(path, maxPixel))
    }

    /// 同步解码（会读文件，不要在主线程的 body 中调用；视图请用 LocalImage / load）
    static func image(_ path: String, maxPixel: CGFloat = 600) -> UIImage? {
        let k = key(path, maxPixel)
        if let img = cache.object(forKey: k) { return img }
        let url = URL(fileURLWithPath: path) as CFURL
        guard let src = CGImageSourceCreateWithURL(url, nil) else { return nil }
        let opts: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixel,
        ]
        guard let cg = CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary) else { return nil }
        let img = UIImage(cgImage: cg)
        cache.setObject(img, forKey: k, cost: cg.bytesPerRow * cg.height)
        return img
    }

    /// 在后台线程解码
    static func load(_ path: String, maxPixel: CGFloat = 600) async -> UIImage? {
        if let img = cached(path, maxPixel: maxPixel) { return img }
        return await Task.detached(priority: .userInitiated) { image(path, maxPixel: maxPixel) }.value
    }

    static func removeAll() { cache.removeAllObjects() }
}

/// 异步显示本地图片：内存命中直接显示，否则在后台解码，期间显示占位
struct LocalImage<Placeholder: View>: View {
    let path: String?
    var maxPixel: CGFloat = 600
    var contentMode: ContentMode = .fill
    @ViewBuilder var placeholder: () -> Placeholder
    @State private var loaded: (key: String, image: UIImage)?

    private var key: String { "\(path ?? "")#\(Int(maxPixel))" }

    private var image: UIImage? {
        guard let path else { return nil }
        if let loaded, loaded.key == key { return loaded.image }
        return ImageCache.cached(path, maxPixel: maxPixel)
    }

    var body: some View {
        Group {
            if let image {
                Image(uiImage: image).resizable().aspectRatio(contentMode: contentMode)
            } else {
                placeholder()
            }
        }
        .task(id: key) {
            guard let path, image == nil else { return }
            let k = key
            if let img = await ImageCache.load(path, maxPixel: maxPixel), !Task.isCancelled {
                loaded = (k, img)
            }
        }
    }
}

enum ImageUtil {
    /// 压缩到长边 ≤ maxSide，JPEG 质量 0.8，写入临时文件，返回 (路径, 宽, 高)
    static func compressToJPEG(_ data: Data, maxSide: CGFloat = 2048) -> (URL, Int, Int)? {
        guard let img = UIImage(data: data) else { return nil }
        let resized = resize(img, maxSide: maxSide)
        guard let jpg = resized.jpegData(compressionQuality: 0.8) else { return nil }
        let url = AppPaths.newOutboxFile(ext: "jpg")
        do { try jpg.write(to: url) } catch { return nil }
        return (url, Int(resized.size.width * resized.scale), Int(resized.size.height * resized.scale))
    }

    static func resize(_ img: UIImage, maxSide: CGFloat) -> UIImage {
        let w = img.size.width * img.scale, h = img.size.height * img.scale
        let longest = max(w, h)
        let scale = longest > maxSide ? maxSide / longest : 1
        let size = CGSize(width: floor(w * scale), height: floor(h * scale))
        let fmt = UIGraphicsImageRendererFormat()
        fmt.scale = 1
        return UIGraphicsImageRenderer(size: size, format: fmt).image { _ in
            img.draw(in: CGRect(origin: .zero, size: size))
        }
    }

    /// 小缩略图（随消息加密发送，对方先显示模糊图）
    static func thumbnail(_ url: URL) -> Data? {
        guard let img = ImageCache.image(url.path, maxPixel: 64) else { return nil }
        return img.jpegData(compressionQuality: 0.5)
    }
}
