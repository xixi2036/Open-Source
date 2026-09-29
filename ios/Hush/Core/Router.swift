import SwiftUI

enum AppTab: Hashable { case chats, contacts, moments, me }

/// 页面路由（每个 Tab 一个 NavigationStack）
enum Route: Hashable {
    case chat(String)               // convId
    case chatFocus(String, String)  // convId, messageId：打开会话并定位、高亮该消息（消息搜索）
    case convSettings(String)       // convId
    case safety(String)             // userId
    case profile(String)            // userId
    case userMoments(String)        // userId
    case momentDetail(String)       // momentId
    case friendRequests
    case groupList
    case addFriend
    case myQR
    case archived
    case momentNotifications
    case editProfile
    case settings
    case blocked
    case about
}

@MainActor @Observable
final class Router {
    static let shared = Router()
    var tab: AppTab = .chats
    var chatPath = NavigationPath()
    var contactsPath = NavigationPath()
    var momentsPath = NavigationPath()
    var mePath = NavigationPath()

    /// 从任意位置打开会话（通知点击、资料页“发消息”）
    func openChat(_ convId: String) {
        tab = .chats
        var p = NavigationPath()
        p.append(Route.chat(convId))
        chatPath = p
    }

    func push(_ r: Route) {
        switch tab {
        case .chats: chatPath.append(r)
        case .contacts: contactsPath.append(r)
        case .moments: momentsPath.append(r)
        case .me: mePath.append(r)
        }
    }

    /// 当前 Tab 返回上一页
    func pop() {
        switch tab {
        case .chats: if !chatPath.isEmpty { chatPath.removeLast() }
        case .contacts: if !contactsPath.isEmpty { contactsPath.removeLast() }
        case .moments: if !momentsPath.isEmpty { momentsPath.removeLast() }
        case .me: if !mePath.isEmpty { mePath.removeLast() }
        }
    }

    func reset() {
        tab = .chats
        chatPath = NavigationPath(); contactsPath = NavigationPath()
        momentsPath = NavigationPath(); mePath = NavigationPath()
    }
}
