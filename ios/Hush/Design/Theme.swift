import SwiftUI
import UIKit

/// 设计令牌（docs/ui-guide.md §1），浅色/深色自动切换。
enum Theme {
    static let accent = dyn(0x2C6BED, 0x5B8DF5)
    static let onAccent = Color.white
    static let background = dyn(0xFFFFFF, 0x121212)
    static let surface = dyn(0xF6F6F6, 0x1E1E1E)
    static let incomingBubble = dyn(0xE9E9EB, 0x2C2C2E)
    static let textPrimary = dyn(0x1B1B1D, 0xE9E9E9)
    static let textSecondary = dyn(0x6B6B70, 0xA0A0A5)
    static let divider = dyn(0xE3E3E3, 0x2C2C2E)
    static let danger = dyn(0xE0413B, 0xFF6961)
    static let success = dyn(0x1FA95F, 0x34C77B)

    static let bubbleRadius: CGFloat = 18
    static let cardRadius: CGFloat = 12

    /// 头像底色：8 种柔和色
    static let avatarPalette: [Color] = [
        0x7FA7E8, 0xE8927F, 0x8CC98A, 0xD49BD8, 0xE8C36F, 0x6FC7C2, 0xB39DDB, 0xF0A0B5,
    ].map { Color(rgb: $0) }

    /// 群聊发送者昵称颜色（比头像色深一些，保证可读）
    static let namePalette: [Color] = [
        0x2F6FD0, 0xC8553D, 0x3E8E41, 0x9C4DA3, 0xB8860B, 0x1F8A85, 0x6A4FB3, 0xC2476A,
    ].map { Color(rgb: $0) }

    static func paletteIndex(for id: String) -> Int {
        var h: UInt32 = 5381
        for b in id.utf8 { h = (h &* 33) &+ UInt32(b) }
        return Int(h % 8)
    }

    private static func dyn(_ light: UInt32, _ dark: UInt32) -> Color {
        Color(UIColor { tc in
            UIColor(rgb: tc.userInterfaceStyle == .dark ? dark : light)
        })
    }
}

enum Fonts {
    static let title = Font.system(size: 20, weight: .semibold)
    static let body = Font.system(size: 16)
    static let secondary = Font.system(size: 13)
    static let timestamp = Font.system(size: 11)
}

extension UIColor {
    convenience init(rgb: UInt32) {
        self.init(red: CGFloat((rgb >> 16) & 0xFF) / 255, green: CGFloat((rgb >> 8) & 0xFF) / 255,
                  blue: CGFloat(rgb & 0xFF) / 255, alpha: 1)
    }
}

extension Color {
    init(rgb: UInt32) { self.init(uiColor: UIColor(rgb: rgb)) }
}
