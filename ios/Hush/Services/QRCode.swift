import SwiftUI
import UIKit
@preconcurrency import AVFoundation
import CoreImage.CIFilterBuiltins
import PhotosUI

enum QRCode {
    static func image(_ payload: String, size: CGFloat = 600) -> UIImage? {
        let f = CIFilter.qrCodeGenerator()
        f.message = Data(payload.utf8)
        f.correctionLevel = "M"
        guard let out = f.outputImage else { return nil }
        let scale = size / out.extent.width
        let scaled = out.transformed(by: CGAffineTransform(scaleX: scale, y: scale))
        guard let cg = CIContext().createCGImage(scaled, from: scaled.extent) else { return nil }
        return UIImage(cgImage: cg)
    }

    /// 从图片中识别二维码
    static func decode(_ image: UIImage) -> String? {
        guard let ci = CIImage(image: image) else { return nil }
        let det = CIDetector(ofType: CIDetectorTypeQRCode, context: nil, options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])
        return (det?.features(in: ci).compactMap { ($0 as? CIQRCodeFeature)?.messageString }.first)
    }

    /// `hush-user:v1:<id>` → user id
    static func userId(from payload: String) -> String? {
        let prefix = "hush-user:v1:"
        guard payload.hasPrefix(prefix) else { return nil }
        let id = String(payload.dropFirst(prefix.count))
        return id.isEmpty ? nil : id
    }
}

struct QRImageView: View {
    let payload: String
    var body: some View {
        if let img = QRCode.image(payload) {
            Image(uiImage: img).interpolation(.none).resizable().scaledToFit()
                .accessibilityIdentifier("qrImage")
        } else {
            Color.gray.opacity(0.2)
        }
    }
}

/// 扫码页：相机扫描（AVCaptureMetadataOutput），也可从相册图片识别（模拟器无相机时可用）
struct QRScannerSheet: View {
    var onResult: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var pick: PhotosPickerItem?
    @State private var error: String?
    private let hasCamera = AVCaptureDevice.default(for: .video) != nil

    var body: some View {
        NavigationStack {
            ZStack {
                Color.black.ignoresSafeArea()
                if hasCamera {
                    CameraScannerView { code in finish(code) }.ignoresSafeArea()
                    RoundedRectangle(cornerRadius: 16).stroke(Color.white, lineWidth: 3).frame(width: 240, height: 240)
                } else {
                    VStack(spacing: 12) {
                        Image(systemName: "camera.metering.unknown").font(.system(size: 48)).foregroundStyle(.white.opacity(0.7))
                        Text("当前设备没有可用的相机").foregroundStyle(.white.opacity(0.8))
                    }
                }
                VStack {
                    Spacer()
                    if let error { Text(error).foregroundStyle(Theme.danger).padding(8).background(Capsule().fill(.black.opacity(0.6))) }
                    PhotosPicker(selection: $pick, matching: .images) {
                        Label("从相册选择二维码", systemImage: "photo")
                            .foregroundStyle(.white)
                            .padding(.horizontal, 20).padding(.vertical, 12)
                            .background(Capsule().fill(Color.white.opacity(0.2)))
                    }
                    .accessibilityIdentifier("scanFromAlbum")
                    .padding(.bottom, 40)
                }
            }
            .navigationTitle("扫一扫")
            .navigationBarTitleDisplayMode(.inline)
            .toolbarColorScheme(.dark, for: .navigationBar)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() }.tint(.white) } }
            .onChange(of: pick) { _, item in
                guard let item else { return }
                Task {
                    if let data = try? await item.loadTransferable(type: Data.self), let img = UIImage(data: data),
                       let code = QRCode.decode(img) {
                        finish(code)
                    } else {
                        error = "未识别到二维码"
                    }
                }
            }
        }
    }

    private func finish(_ code: String) {
        dismiss()
        onResult(code)
    }
}

struct CameraScannerView: UIViewControllerRepresentable {
    var onCode: (String) -> Void

    func makeUIViewController(context: Context) -> ScannerController {
        let vc = ScannerController()
        vc.onCode = onCode
        return vc
    }
    func updateUIViewController(_ vc: ScannerController, context: Context) {}

    final class ScannerController: UIViewController, AVCaptureMetadataOutputObjectsDelegate {
        var onCode: ((String) -> Void)?
        private let session = AVCaptureSession()
        private var preview: AVCaptureVideoPreviewLayer?
        private var done = false

        override func viewDidLoad() {
            super.viewDidLoad()
            guard let dev = AVCaptureDevice.default(for: .video), let input = try? AVCaptureDeviceInput(device: dev),
                  session.canAddInput(input) else { return }
            session.addInput(input)
            let out = AVCaptureMetadataOutput()
            guard session.canAddOutput(out) else { return }
            session.addOutput(out)
            out.setMetadataObjectsDelegate(self, queue: .main)
            out.metadataObjectTypes = [.qr]
            let layer = AVCaptureVideoPreviewLayer(session: session)
            layer.videoGravity = .resizeAspectFill
            view.layer.addSublayer(layer)
            preview = layer
            let s = session
            DispatchQueue.global(qos: .userInitiated).async { s.startRunning() }
        }

        override func viewDidLayoutSubviews() {
            super.viewDidLayoutSubviews()
            preview?.frame = view.bounds
        }

        override func viewWillDisappear(_ animated: Bool) {
            super.viewWillDisappear(animated)
            let s = session
            DispatchQueue.global().async { s.stopRunning() }
        }

        // 代理队列设为 .main，回调一定在主线程
        nonisolated func metadataOutput(_ output: AVCaptureMetadataOutput, didOutput objects: [AVMetadataObject], from connection: AVCaptureConnection) {
            let code = (objects.first as? AVMetadataMachineReadableCodeObject)?.stringValue
            MainActor.assumeIsolated {
                guard !done, let code else { return }
                done = true
                onCode?(code)
            }
        }
    }
}
