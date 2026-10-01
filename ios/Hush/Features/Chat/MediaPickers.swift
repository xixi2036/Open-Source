import SwiftUI
import UIKit
import AVFoundation
import AVKit

/// 应用内拍照（系统相机界面）。只拍照片；结果写成临时文件交给调用方（nil = 取消）。
struct CameraPicker: UIViewControllerRepresentable {
    var onFinish: (URL?) -> Void
    @Environment(\.dismiss) private var dismiss

    enum Authorization { case granted, denied, undetermined }

    /// 设备有摄像头才显示「拍照」入口（模拟器没有）
    static var isAvailable: Bool { UIImagePickerController.isSourceTypeAvailable(.camera) }

    static func authorization(_ status: AVAuthorizationStatus = AVCaptureDevice.authorizationStatus(for: .video)) -> Authorization {
        switch status {
        case .authorized: return .granted
        case .notDetermined: return .undetermined
        default: return .denied
        }
    }

    static func requestAccess() async -> Bool { await AVCaptureDevice.requestAccess(for: .video) }

    func makeUIViewController(context: Context) -> UIImagePickerController {
        let vc = UIImagePickerController()
        vc.sourceType = .camera
        vc.mediaTypes = ["public.image"]
        vc.delegate = context.coordinator
        return vc
    }

    func updateUIViewController(_ vc: UIImagePickerController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    final class Coordinator: NSObject, UIImagePickerControllerDelegate, UINavigationControllerDelegate {
        let parent: CameraPicker
        init(_ parent: CameraPicker) { self.parent = parent }

        func imagePickerController(_ picker: UIImagePickerController, didFinishPickingMediaWithInfo info: [UIImagePickerController.InfoKey: Any]) {
            let image = info[.originalImage] as? UIImage
            parent.dismiss()
            guard let image else { parent.onFinish(nil); return }
            let finish = parent.onFinish
            // 编码放到后台：全分辨率照片的 JPEG 编码要几百毫秒
            Task.detached(priority: .userInitiated) {
                let url = AppPaths.newOutboxFile(ext: "jpg")
                let ok = (try? image.jpegData(compressionQuality: 0.95)?.write(to: url)) != nil
                await MainActor.run { finish(ok ? url : nil) }
            }
        }

        func imagePickerControllerDidCancel(_ picker: UIImagePickerController) {
            parent.dismiss()
            parent.onFinish(nil)
        }
    }
}

/// 内置视频播放（AVPlayer）
struct VideoPlayerScreen: View {
    let path: String
    @Environment(\.dismiss) private var dismiss
    @State private var player: AVPlayer?

    var body: some View {
        ZStack(alignment: .topLeading) {
            Color.black.ignoresSafeArea()
            if let player {
                VideoPlayer(player: player).ignoresSafeArea(edges: .bottom)
                    .accessibilityIdentifier("videoPlayer")
            }
            Button { dismiss() } label: {
                Image(systemName: "xmark").font(.system(size: 18, weight: .semibold))
                    .foregroundStyle(.white).frame(width: 44, height: 44)
                    .background(Circle().fill(Color.white.opacity(0.18)))
            }
            .accessibilityLabel("关闭")
            .accessibilityIdentifier("closeVideo")
            .padding(.leading, 16)
        }
        .onAppear {
            VoicePlayer.shared.stop()
            try? AVAudioSession.sharedInstance().setCategory(.playback, mode: .moviePlayback)
            try? AVAudioSession.sharedInstance().setActive(true)
            let p = AVPlayer(url: URL(fileURLWithPath: path))
            player = p
            p.play()
        }
        .onDisappear {
            player?.pause()
            player = nil
            if !CallManager.shared.isLive {
                try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
            }
        }
        .onChange(of: CallManager.shared.isLive) { _, live in if live { player?.pause() } }
    }
}

struct VideoItem: Identifiable {
    let path: String
    var id: String { path }
}

/// 文本部分选择：在面板里用可选择的文本视图显示整条消息，可拖动选择任意一段后复制
struct TextSelectSheet: View {
    let text: String
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            SelectableText(text: text)
                .padding(.horizontal, 16)
                .background(Theme.background)
                .navigationTitle("选择文字")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("完成") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("复制全部") {
                            UIPasteboard.general.string = text
                            HushCore.shared.toast = "已复制"
                            dismiss()
                        }
                        .accessibilityIdentifier("copyAllText")
                    }
                }
        }
        .presentationDetents([.medium, .large])
    }
}

/// 只读、可选择的文本视图（系统的选择手柄与「拷贝」菜单）
struct SelectableText: UIViewRepresentable {
    let text: String

    func makeUIView(context: Context) -> UITextView {
        let v = UITextView()
        v.isEditable = false
        v.isSelectable = true
        v.backgroundColor = .clear
        v.textColor = UIColor(Theme.textPrimary)
        v.tintColor = UIColor(Theme.accent)
        v.font = UIFontMetrics(forTextStyle: .body).scaledFont(for: .systemFont(ofSize: 18))
        v.adjustsFontForContentSizeCategory = true
        v.dataDetectorTypes = []
        v.textContainerInset = UIEdgeInsets(top: 16, left: 0, bottom: 16, right: 0)
        v.alwaysBounceVertical = true
        v.accessibilityIdentifier = "selectableText"
        v.text = text
        return v
    }

    func updateUIView(_ v: UITextView, context: Context) {
        if v.text != text { v.text = text }
    }
}
