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
                    // C5：只有当前页和前后各一页持有解码后的原图，其余页释放
                    ZoomableImage(path: p, active: Self.shouldLoad(page: i, current: index)).tag(i).ignoresSafeArea()
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
                .accessibilityLabel("关闭")
                .accessibilityIdentifier("closeViewer")
                Spacer()
                Button { save() } label: {
                    Image(systemName: "square.and.arrow.down").font(.system(size: 18, weight: .semibold))
                        .foregroundStyle(.white).frame(width: 44, height: 44)
                        .background(Circle().fill(Color.white.opacity(0.15)))
                }
                .accessibilityLabel("保存到相册")
                .accessibilityIdentifier("saveImage")
            }
            .padding(.horizontal, 16)
        }
        .statusBarHidden()
    }

    /// 当前页与前后各一页才加载原图
    static func shouldLoad(page: Int, current: Int) -> Bool { abs(page - current) <= 1 }

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
    /// 是否加载原图（不在当前页附近时释放，避免一次把整个会话的图片都解码进内存）
    var active = true

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
        iv.accessibilityLabel = "图片"
        sv.addSubview(iv)
        context.coordinator.imageView = iv
        let dbl = UITapGestureRecognizer(target: context.coordinator, action: #selector(Coordinator.doubleTap(_:)))
        dbl.numberOfTapsRequired = 2
        sv.addGestureRecognizer(dbl)
        context.coordinator.setActive(active, path: path, scroll: sv)
        return sv
    }

    func updateUIView(_ sv: UIScrollView, context: Context) {
        context.coordinator.setActive(active, path: path, scroll: sv)
    }

    func makeCoordinator() -> Coordinator { Coordinator() }

    @MainActor
    final class Coordinator: NSObject, UIScrollViewDelegate {
        weak var imageView: UIImageView?
        private var loadedPath: String?
        private var task: Task<Void, Never>?

        func setActive(_ active: Bool, path: String, scroll: UIScrollView) {
            guard active else {
                task?.cancel(); task = nil
                loadedPath = nil
                imageView?.image = nil
                if scroll.zoomScale != 1 { scroll.zoomScale = 1 }
                return
            }
            guard loadedPath != path else { return }
            loadedPath = path
            task?.cancel()
            // 原图在后台解码，避免打开大图时卡住主线程
            task = Task { @MainActor [weak self] in
                let img = await Task.detached(priority: .userInitiated) { UIImage(contentsOfFile: path)?.preparingForDisplay() }.value
                guard !Task.isCancelled, let self, self.loadedPath == path else { return }
                self.imageView?.image = img
            }
        }

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
