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
    /// 压缩到长边 ≤ maxSide，JPEG 质量 0.8，写入临时文件，返回 (路径, 宽, 高)。
    /// C4：用 ImageIO 直接按目标尺寸降采样解码，不把 4800 万像素的原图整张解进内存；可在任意线程调用。
    nonisolated static func compressToJPEG(_ data: Data, maxSide: CGFloat = 2048) -> (URL, Int, Int)? {
        guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        return compress(src, maxSide: maxSide)
    }

    /// 同上，直接从文件读取（拍照得到的文件等）
    nonisolated static func compressToJPEG(fileURL: URL, maxSide: CGFloat = 2048) -> (URL, Int, Int)? {
        guard let src = CGImageSourceCreateWithURL(fileURL as CFURL, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        return compress(src, maxSide: maxSide)
    }

    private nonisolated static func compress(_ src: CGImageSource, maxSide: CGFloat) -> (URL, Int, Int)? {
        guard let cg = downsample(src, maxPixel: maxSide) else { return nil }
        let url = AppPaths.newOutboxFile(ext: "jpg")
        guard let dst = CGImageDestinationCreateWithURL(url as CFURL, "public.jpeg" as CFString, 1, nil) else { return nil }
        // 方向已在降采样时应用（WithTransform），输出不带 EXIF：位置等元数据不会随图片发出去
        CGImageDestinationAddImage(dst, cg, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
        guard CGImageDestinationFinalize(dst) else { return nil }
        return (url, cg.width, cg.height)
    }

    /// 按最长边降采样解码（已应用 EXIF 方向）；原图不大于 maxPixel 时按原尺寸解码
    nonisolated static func downsample(_ src: CGImageSource, maxPixel: CGFloat) -> CGImage? {
        let opts: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: max(1, Int(maxPixel)),
        ]
        return CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary)
    }

    /// 降采样后的像素尺寸（不解码图像数据，只读头部）
    nonisolated static func pixelSize(_ src: CGImageSource) -> CGSize? {
        guard let props = CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [CFString: Any],
              let w = props[kCGImagePropertyPixelWidth] as? CGFloat, let h = props[kCGImagePropertyPixelHeight] as? CGFloat else { return nil }
        return CGSize(width: w, height: h)
    }

    nonisolated static func resize(_ img: UIImage, maxSide: CGFloat) -> UIImage {
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

    /// 小缩略图（随消息加密发送，对方先显示模糊图）。会读文件并解码，不要在主线程调用。
    nonisolated static func thumbnail(_ url: URL, maxPixel: CGFloat = 64) -> Data? {
        guard let src = CGImageSourceCreateWithURL(url as CFURL, nil), let cg = downsample(src, maxPixel: maxPixel) else { return nil }
        return UIImage(cgImage: cg).jpegData(compressionQuality: 0.5)
    }
}
