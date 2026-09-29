import SwiftUI
import UIKit
import Photos

/// 全屏图片查看：双指缩放、双击放大、保存到相册
struct ImageViewer: View {
    let paths: [String]
    @State var index: Int
    @Environment(\.dismiss) private var dismiss

    init(paths: [String], index: Int = 0) {
        self.paths = paths
        _index = State(initialValue: index)
    }

    var body: some View {
        ZStack(alignment: .top) {
            Color.black.ignoresSafeArea()
            TabView(selection: $index) {
                ForEach(Array(paths.enumerated()), id: \.offset) { i, p in
                    ZoomableImage(path: p).tag(i).ignoresSafeArea()
                }
            }
            .tabViewStyle(.page(indexDisplayMode: paths.count > 1 ? .automatic : .never))
            .ignoresSafeArea()
            HStack {
                Button { dismiss() } label: {
                    Image(systemName: "xmark").font(.system(size: 18, weight: .semibold))
                        .foregroundStyle(.white).frame(width: 44, height: 44)
                        .background(Circle().fill(Color.white.opacity(0.15)))
                }
                .accessibilityIdentifier("closeViewer")
                Spacer()
                Button { save() } label: {
                    Image(systemName: "square.and.arrow.down").font(.system(size: 18, weight: .semibold))
                        .foregroundStyle(.white).frame(width: 44, height: 44)
                        .background(Circle().fill(Color.white.opacity(0.15)))
                }
                .accessibilityIdentifier("saveImage")
            }
            .padding(.horizontal, 16)
        }
        .statusBarHidden()
    }

    private func save() {
        guard index < paths.count else { return }
        let url = URL(fileURLWithPath: paths[index])
        Task {
            let status = await PHPhotoLibrary.requestAuthorization(for: .addOnly)
            guard status == .authorized || status == .limited else {
                HushCore.shared.toast = "没有相册权限，请在「设置」中允许 Hush 添加照片"
                return
            }
            do {
                try await PHPhotoLibrary.shared().performChanges {
                    _ = PHAssetChangeRequest.creationRequestForAssetFromImage(atFileURL: url)
                }
                HushCore.shared.toast = "已保存到相册"
            } catch {
                HushCore.shared.toast = "保存失败"
            }
        }
    }
}

struct ZoomableImage: UIViewRepresentable {
    let path: String

    func makeUIView(context: Context) -> UIScrollView {
        let sv = UIScrollView()
        sv.minimumZoomScale = 1
        sv.maximumZoomScale = 5
        sv.delegate = context.coordinator
        sv.showsHorizontalScrollIndicator = false
        sv.showsVerticalScrollIndicator = false
        sv.backgroundColor = .black
        let iv = UIImageView()
        iv.contentMode = .scaleAspectFit
        iv.frame = sv.bounds
        iv.autoresizingMask = [.flexibleWidth, .flexibleHeight]
        iv.isAccessibilityElement = true
        iv.accessibilityIdentifier = "zoomImage"
        sv.addSubview(iv)
        context.coordinator.imageView = iv
        // 原图在后台解码，避免打开大图时卡住主线程
        let path = path
        Task { @MainActor [weak iv] in
            let img = await Task.detached(priority: .userInitiated) { UIImage(contentsOfFile: path)?.preparingForDisplay() }.value
            iv?.image = img
        }
        let dbl = UITapGestureRecognizer(target: context.coordinator, action: #selector(Coordinator.doubleTap(_:)))
        dbl.numberOfTapsRequired = 2
        sv.addGestureRecognizer(dbl)
        return sv
    }

    func updateUIView(_ uiView: UIScrollView, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator() }

    final class Coordinator: NSObject, UIScrollViewDelegate {
        weak var imageView: UIImageView?
        func viewForZooming(in scrollView: UIScrollView) -> UIView? { imageView }
        @objc func doubleTap(_ g: UITapGestureRecognizer) {
            guard let sv = g.view as? UIScrollView else { return }
            sv.setZoomScale(sv.zoomScale > 1 ? 1 : 2.5, animated: true)
        }
    }
}

struct ImageViewerItem: Identifiable {
    let id = UUID()
    let paths: [String]
    let index: Int
}
