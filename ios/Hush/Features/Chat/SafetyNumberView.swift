import SwiftUI

/// 安全码：60 位数字（12 组 × 5 位，3 列）、二维码、扫码核验、标记已核验
struct SafetyNumberView: View {
    let userId: String
    var core = HushCore.shared
    @State private var sn: SafetyNumber?
    @State private var error: String?
    @State private var showScanner = false
    @State private var scanResult: Bool?

    var body: some View {
        ScrollView {
            VStack(spacing: 20) {
                if let sn {
                    if sn.verified {
                        Label("已核验", systemImage: "checkmark.shield.fill")
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(Theme.success)
                            .padding(.horizontal, 14).padding(.vertical, 6)
                            .background(Capsule().fill(Theme.success.opacity(0.12)))
                            .accessibilityIdentifier("verifiedBadge")
                    }
                    QRImageView(payload: sn.qrPayload)
                        .frame(width: 200, height: 200)
                        .padding(12)
                        .background(Color.white)
                        .clipShape(RoundedRectangle(cornerRadius: 12))
                    digitsGrid(sn.digits)
                    Text("将上面的数字与 \(core.contacts.displayName(userId)) 设备上显示的进行比对，或扫描对方的二维码。一致即说明你们的通信是端到端加密且未被中间人篡改。")
                        .font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                        .multilineTextAlignment(.center).padding(.horizontal, 24)
                    if let scanResult {
                        Label(scanResult ? "核验成功：安全码一致" : "核验失败：安全码不一致",
                              systemImage: scanResult ? "checkmark.circle.fill" : "xmark.octagon.fill")
                            .foregroundStyle(scanResult ? Theme.success : Theme.danger)
                    }
                    VStack(spacing: 12) {
                        Button { showScanner = true } label: { Label("扫码核验", systemImage: "qrcode.viewfinder") }
                            .buttonStyle(PrimaryButtonStyle())
                            .accessibilityIdentifier("scanVerify")
                        Button(sn.verified ? "取消核验" : "标记为已核验") { toggleVerified(!sn.verified) }
                            .font(.system(size: 16, weight: .medium))
                            .frame(maxWidth: .infinity, minHeight: 46)
                            .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.surface))
                            .accessibilityIdentifier("toggleVerified")
                    }
                    .padding(.horizontal, 24)
                } else if let error {
                    EmptyStateView(systemImage: "exclamationmark.triangle", text: error)
                } else {
                    ProgressView().padding(60)
                }
            }
            .padding(.vertical, 24)
        }
        .background(Theme.background)
        .navigationTitle("安全码")
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .sheet(isPresented: $showScanner) {
            QRScannerSheet { code in
                do {
                    scanResult = try core.contacts.verifySafetyQr(userId, payload: code)
                    Task { await load() }
                } catch { core.toast = describe(error) }
            }
        }
    }

    private func digitsGrid(_ digits: String) -> some View {
        let chars = Array(digits)
        let groups = stride(from: 0, to: chars.count, by: 5).map { String(chars[$0..<min($0 + 5, chars.count)]) }
        return LazyVGrid(columns: Array(repeating: GridItem(.flexible()), count: 3), spacing: 12) {
            ForEach(Array(groups.enumerated()), id: \.offset) { _, g in
                Text(g).font(.system(size: 20, weight: .medium, design: .monospaced)).foregroundStyle(Theme.textPrimary)
            }
        }
        .padding(16)
        .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.surface))
        .padding(.horizontal, 24)
        .accessibilityIdentifier("safetyDigits")
    }

    private func load() async {
        do {
            sn = try await core.contacts.safetyNumber(userId)
        } catch { self.error = describe(error) }
    }

    private func toggleVerified(_ v: Bool) {
        do {
            try core.contacts.setVerified(userId, v)
            Task { await load() }
        } catch { core.toast = describe(error) }
    }
}
