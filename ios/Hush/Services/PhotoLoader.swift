import SwiftUI
import PhotosUI

enum PhotoLoader {
    /// 读取相册选择项并压缩为 JPEG（长边 ≤ maxSide）
    static func load(_ item: PhotosPickerItem, maxSide: CGFloat = 2048) async -> (url: URL, width: Int, height: Int)? {
        guard let data = try? await item.loadTransferable(type: Data.self) else { return nil }
        guard let r = ImageUtil.compressToJPEG(data, maxSide: maxSide) else { return nil }
        return (r.0, r.1, r.2)
    }
}
