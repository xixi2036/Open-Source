import SwiftUI
import PhotosUI

/// 头像选择：点击从相册选图，压缩后把临时文件地址写入 avatarURL
struct AvatarPickerField<Current: View>: View {
    let size: CGFloat
    @Binding var avatarURL: URL?
    @ViewBuilder var current: () -> Current
    @State private var pick: PhotosPickerItem?
    @State private var showPicker = false

    var body: some View {
        Button { showPicker = true } label: {
            ZStack(alignment: .bottomTrailing) {
                Group {
                    if let url = avatarURL {
                        LocalImage(path: url.path, maxPixel: size * 3) { current() }
                    } else {
                        current()
                    }
                }
                .frame(width: size, height: size)
                .clipShape(Circle())
                Image(systemName: "camera.fill")
                    .font(.system(size: size * 0.13)).foregroundStyle(.white)
                    .frame(width: size * 0.29, height: size * 0.29).background(Circle().fill(Theme.accent))
            }
        }
        .buttonStyle(.plain)
        .photosPicker(isPresented: $showPicker, selection: $pick, matching: .images)
        .onChange(of: pick) { _, item in
            guard let item else { return }
            Task { avatarURL = await PhotoLoader.load(item, maxSide: 640)?.url }
        }
    }
}
