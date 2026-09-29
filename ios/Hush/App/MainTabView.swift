import SwiftUI

/// 主界面四个 Tab，各自一个 NavigationStack
struct MainTabView: View {
    @Bindable var router = Router.shared
    var core = HushCore.shared

    var body: some View {
        TabView(selection: $router.tab) {
            NavigationStack(path: $router.chatPath) {
                ConversationListView().routeDestinations()
            }
            .tabItem { Label("聊天", systemImage: "message.fill") }
            .badge(core.chats.totalUnread)
            .tag(AppTab.chats)

            NavigationStack(path: $router.contactsPath) {
                ContactsView().routeDestinations()
            }
            .tabItem { Label("通讯录", systemImage: "person.2.fill") }
            .badge(core.contacts.pendingRequestCount)
            .tag(AppTab.contacts)

            NavigationStack(path: $router.momentsPath) {
                MomentsView().routeDestinations()
            }
            .tabItem { Label("朋友圈", systemImage: "camera.aperture") }
            .badge(core.moments.newNoteCount)
            .tag(AppTab.moments)

            NavigationStack(path: $router.mePath) {
                MeView().routeDestinations()
            }
            .tabItem { Label("我", systemImage: "person.crop.circle.fill") }
            .tag(AppTab.me)
        }
    }
}

extension View {
    func routeDestinations() -> some View {
        navigationDestination(for: Route.self) { RouteView(route: $0) }
    }
}
