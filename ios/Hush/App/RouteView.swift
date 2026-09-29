import SwiftUI

/// Route → 页面
struct RouteView: View {
    let route: Route
    var body: some View {
        switch route {
        case let .chat(id): ChatView(convId: id).id(id)
        case let .chatFocus(id, mid): ChatView(convId: id, focusMessageId: mid).id("\(id)#\(mid)")
        case let .convSettings(id): ConversationSettingsView(convId: id)
        case let .safety(uid): SafetyNumberView(userId: uid)
        case let .profile(uid): ProfileView(userId: uid)
        case let .userMoments(uid): UserMomentsView(userId: uid)
        case let .momentDetail(id): MomentDetailView(momentId: id)
        case .friendRequests: FriendRequestsView()
        case .groupList: GroupListView()
        case .addFriend: AddFriendView()
        case .myQR: MyQRCodeView()
        case .archived: ArchivedListView()
        case .momentNotifications: MomentNotificationsView()
        case .editProfile: EditProfileView()
        case .settings: SettingsView()
        case .blocked: BlockedListView()
        case .about: AboutView()
        }
    }
}
