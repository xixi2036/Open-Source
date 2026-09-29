import SwiftUI

/// 圆形头像：有图显示图，否则按 id 哈希取底色 + 昵称首字
struct AvatarView: View {
    let seed: String
    let name: String
    let avatarId: String?
    var size: CGFloat = 40
    var core = HushCore.shared

    var body: some View {
        ZStack {
            if let aid = avatarId, let path = core.media.avatarPaths[aid] {
                LocalImage(path: path, maxPixel: size * 3) { fallback }
            } else {
                fallback
            }
        }
        .frame(width: size, height: size)
        .clipShape(Circle())
        .task(id: avatarId) {
            if let aid = avatarId { await core.loadAvatar(aid) }
        }
        .accessibilityHidden(true)
    }

    private var fallback: some View {
        ZStack {
            Theme.avatarPalette[Theme.paletteIndex(for: seed)]
            Text(Self.initial(name))
                .font(.system(size: size * 0.42, weight: .medium))
                .foregroundStyle(.white)
        }
    }

    static func initial(_ name: String) -> String {
        guard let c = name.trimmingCharacters(in: .whitespaces).first else { return "?" }
        return String(c).uppercased()
    }
}

/// 按 user_id 显示用户头像
struct UserAvatar: View {
    let userId: String
    var size: CGFloat = 40
    var core = HushCore.shared

    var body: some View {
        let p = core.contacts.profile(userId)
        AvatarView(seed: userId, name: p?.name ?? core.contacts.displayName(userId), avatarId: p?.avatarId, size: size)
    }
}

/// 会话头像（单聊取对方，群聊取群头像）
struct ConversationAvatar: View {
    let convId: String
    var size: CGFloat = 56
    var core = HushCore.shared

    var body: some View {
        if convId.hasPrefix("u:") {
            UserAvatar(userId: String(convId.dropFirst(2)), size: size)
        } else {
            let g = core.contacts.group(String(convId.dropFirst(2)))
            ZStack {
                AvatarView(seed: convId, name: g?.name ?? "群", avatarId: g?.avatarId, size: size)
                if g?.avatarId == nil {
                    Circle().fill(Theme.avatarPalette[Theme.paletteIndex(for: convId)])
                    Image(systemName: "person.3.fill")
                        .font(.system(size: size * 0.34))
                        .foregroundStyle(.white)
                }
            }
            .frame(width: size, height: size)
        }
    }
}
