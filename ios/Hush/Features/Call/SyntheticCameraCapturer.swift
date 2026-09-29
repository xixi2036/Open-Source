import Foundation
import CoreGraphics
import CoreText
import CoreVideo
import UIKit
import WebRTC

/// 无摄像头设备（如模拟器）时的视频源：生成带时间与名称的动态测试画面，保证视频链路可用且对方能看到明确提示。
final class SyntheticCameraCapturer: RTCVideoCapturer {
    private let width = 640
    private let height = 480
    private let label: String
    private let queue = DispatchQueue(label: "hush.synthetic-camera")
    private var timer: DispatchSourceTimer?
    private var startedAt = CACurrentMediaTime()

    init(delegate: RTCVideoCapturerDelegate, label: String) {
        self.label = label
        super.init(delegate: delegate)
    }

    func start(fps: Int = 15) {
        stop()
        startedAt = CACurrentMediaTime()
        let t = DispatchSource.makeTimerSource(queue: queue)
        t.schedule(deadline: .now(), repeating: .milliseconds(1000 / fps))
        t.setEventHandler { [weak self] in self?.emitFrame() }
        t.resume()
        timer = t
    }

    func stop() {
        timer?.cancel()
        timer = nil
    }

    private func emitFrame() {
        guard let buffer = render() else { return }
        let ts = Int64(CACurrentMediaTime() * 1_000_000_000)
        let frame = RTCVideoFrame(buffer: RTCCVPixelBuffer(pixelBuffer: buffer), rotation: ._0, timeStampNs: ts)
        delegate?.capturer(self, didCapture: frame)
    }

    private func render() -> CVPixelBuffer? {
        var pb: CVPixelBuffer?
        let attrs = [kCVPixelBufferIOSurfacePropertiesKey: [:]] as CFDictionary
        guard CVPixelBufferCreate(nil, width, height, kCVPixelFormatType_32BGRA, attrs, &pb) == kCVReturnSuccess,
              let buffer = pb else { return nil }
        CVPixelBufferLockBaseAddress(buffer, [])
        defer { CVPixelBufferUnlockBaseAddress(buffer, []) }
        guard let ctx = CGContext(data: CVPixelBufferGetBaseAddress(buffer), width: width, height: height,
                                  bitsPerComponent: 8, bytesPerRow: CVPixelBufferGetBytesPerRow(buffer),
                                  space: CGColorSpaceCreateDeviceRGB(),
                                  bitmapInfo: CGImageAlphaInfo.premultipliedFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue)
        else { return nil }

        let t = CACurrentMediaTime() - startedAt
        // 缓慢变化的蓝紫渐变背景
        let hue = CGFloat((t / 20).truncatingRemainder(dividingBy: 1))
        let c1 = hsb(0.60 + hue * 0.15, 0.75, 0.85), c2 = hsb(0.72 + hue * 0.15, 0.70, 0.45)
        if let g = CGGradient(colorsSpace: CGColorSpaceCreateDeviceRGB(), colors: [c1, c2] as CFArray, locations: [0, 1]) {
            ctx.drawLinearGradient(g, start: .zero, end: CGPoint(x: width, y: height), options: [])
        }
        // 绕圈移动的光点，直观证明画面在实时刷新
        let r = CGFloat(min(width, height)) * 0.28
        let cx = CGFloat(width) / 2 + r * CGFloat(cos(t * 1.6)), cy = CGFloat(height) / 2 + r * CGFloat(sin(t * 1.6))
        ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 0.85))
        ctx.fillEllipse(in: CGRect(x: cx - 18, y: cy - 18, width: 36, height: 36))

        let clock = Self.clockFormatter.string(from: Date())
        drawText(label, size: 40, weight: .bold, y: CGFloat(height) * 0.58, in: ctx)
        drawText("模拟摄像头 · \(clock)", size: 26, weight: .regular, y: CGFloat(height) * 0.40, in: ctx)
        return buffer
    }

    private func drawText(_ s: String, size: CGFloat, weight: UIFont.Weight, y: CGFloat, in ctx: CGContext) {
        let font = UIFont.systemFont(ofSize: size, weight: weight)
        let str = NSAttributedString(string: s, attributes: [.font: font, .foregroundColor: UIColor.white])
        let line = CTLineCreateWithAttributedString(str)
        let w = CTLineGetTypographicBounds(line, nil, nil, nil)
        ctx.textPosition = CGPoint(x: (CGFloat(width) - CGFloat(w)) / 2, y: y)
        CTLineDraw(line, ctx)
    }

    private func hsb(_ h: CGFloat, _ s: CGFloat, _ b: CGFloat) -> CGColor {
        UIColor(hue: h.truncatingRemainder(dividingBy: 1), saturation: s, brightness: b, alpha: 1).cgColor
    }

    private static let clockFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "HH:mm:ss"
        return f
    }()
}

/// 记录远端视频最后一帧的到达时间，用于判断对方是否在发送画面
final class VideoFrameWatcher: NSObject, RTCVideoRenderer {
    private let lock = NSLock()
    private var last: CFTimeInterval = 0

    func setSize(_ size: CGSize) {}

    func renderFrame(_ frame: RTCVideoFrame?) {
        guard frame != nil else { return }
        lock.lock(); last = CACurrentMediaTime(); lock.unlock()
    }

    /// 最近 window 秒内是否收到过帧
    func hasRecentFrames(within window: CFTimeInterval = 1.5) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return last > 0 && CACurrentMediaTime() - last < window
    }
}
