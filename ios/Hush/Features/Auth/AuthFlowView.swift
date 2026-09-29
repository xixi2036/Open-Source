import SwiftUI
import Combine

enum AuthStep: Hashable { case server, email(server: String), code(server: String, email: String) }

/// 登录流程：欢迎 → 服务器地址 → 邮箱 → 验证码（→ 资料页由 RootView 按 phase 切换）
struct AuthFlowView: View {
    @State private var path: [AuthStep] = []
    @State private var server = HushCore.shared.serverURL
    @State private var email = ""
    var core = HushCore.shared

    var body: some View {
        NavigationStack(path: $path) {
            WelcomeView { path.append(.server) }
                .navigationDestination(for: AuthStep.self) { step in
                    switch step {
                    case .server: ServerStepView(server: $server) { srv in path.append(.email(server: srv)) }
                    case let .email(srv): EmailStepView(server: srv, email: $email) { e in path.append(.code(server: srv, email: e)) }
                    case let .code(srv, e): CodeStepView(server: srv, email: e)
                    }
                }
        }
        .tint(Theme.accent)
    }
}

struct WelcomeView: View {
    var onStart: () -> Void
    var core = HushCore.shared
    var body: some View {
        VStack(spacing: 20) {
            Spacer()
            Image("Logo")
                .resizable().scaledToFit()
                .frame(width: 120, height: 120)
                .clipShape(RoundedRectangle(cornerRadius: 28, style: .continuous))
            Text("Hush 密语").font(.system(size: 30, weight: .bold)).foregroundStyle(Theme.textPrimary)
            Text("端到端加密的私密聊天与朋友圈").font(Fonts.body).foregroundStyle(Theme.textSecondary)
            if let r = core.loggedOutReason {
                Text(r).font(Fonts.secondary).foregroundStyle(Theme.danger)
            }
            Spacer()
            Button("开始使用", action: onStart)
                .buttonStyle(PrimaryButtonStyle())
                .accessibilityIdentifier("startButton")
            Text("继续即表示你同意在本地部署的服务器上注册账号")
                .font(Fonts.timestamp).foregroundStyle(Theme.textSecondary)
        }
        .padding(24)
        .background(Theme.background)
    }
}

struct ServerStepView: View {
    @Binding var server: String
    var onNext: (String) -> Void
    var body: some View {
        AuthPage(title: "服务器地址", subtitle: "本地部署的 Hush 服务器地址") {
            TextField("http://127.0.0.1:8080", text: $server)
                .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                .authField()
                .accessibilityIdentifier("serverField")
        } footer: {
            Button("下一步") { onNext(server.trimmingCharacters(in: .whitespaces)) }
                .buttonStyle(PrimaryButtonStyle(enabled: valid))
                .disabled(!valid)
                .accessibilityIdentifier("serverNext")
        }
    }
    private var valid: Bool {
        let s = server.trimmingCharacters(in: .whitespaces)
        return s.hasPrefix("http://") || s.hasPrefix("https://")
    }
}

struct EmailStepView: View {
    let server: String
    @Binding var email: String
    var onNext: (String) -> Void
    @State private var loading = false
    @State private var error: String?
    @FocusState private var focused: Bool

    var body: some View {
        AuthPage(title: "输入你的邮箱", subtitle: "我们会向该邮箱发送 6 位验证码") {
            TextField("you@example.com", text: $email)
                .keyboardType(.emailAddress).textInputAutocapitalization(.never).autocorrectionDisabled()
                .textContentType(.emailAddress)
                .authField()
                .focused($focused)
                .accessibilityIdentifier("emailField")
            if let error { Text(error).font(Fonts.secondary).foregroundStyle(Theme.danger) }
        } footer: {
            Button {
                submit()
            } label: {
                if loading { ProgressView().tint(.white) } else { Text("下一步") }
            }
            .buttonStyle(PrimaryButtonStyle(enabled: valid && !loading))
            .disabled(!valid || loading)
            .accessibilityIdentifier("emailNext")
        }
        .onAppear { focused = true }
    }

    private var valid: Bool { email.contains("@") && email.contains(".") }

    private func submit() {
        loading = true; error = nil
        Task {
            do {
                let e = email.trimmingCharacters(in: .whitespaces).lowercased()
                try await HushCore.shared.requestCode(server: server.trimmingCharacters(in: .whitespaces), email: e)
                onNext(e)
            } catch { self.error = describe(error) }
            loading = false
        }
    }
}

struct CodeStepView: View {
    let server: String
    let email: String
    @State private var code = ""
    @State private var loading = false
    @State private var error: String?
    @State private var countdown = 60
    @FocusState private var focused: Bool
    private let timer = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    var body: some View {
        AuthPage(title: "输入验证码", subtitle: "验证码已发送至 \(email)") {
            ZStack {
                TextField("", text: $code)
                    .keyboardType(.numberPad)
                    .textContentType(.oneTimeCode)
                    .focused($focused)
                    .foregroundStyle(.clear)
                    .tint(.clear)
                    .opacity(0.02)
                    .accessibilityIdentifier("codeField")
                    .onChange(of: code) { _, v in
                        let digits = String(v.filter(\.isNumber).prefix(6))
                        if digits != v { code = digits }
                        if digits.count == 6 && !loading { submit() }
                    }
                HStack(spacing: 10) {
                    ForEach(0..<6, id: \.self) { i in
                        let ch = i < code.count ? String(Array(code)[i]) : ""
                        Text(ch)
                            .font(.system(size: 26, weight: .semibold, design: .monospaced))
                            .frame(width: 46, height: 56)
                            .background(RoundedRectangle(cornerRadius: 10).fill(Theme.surface))
                            .overlay(RoundedRectangle(cornerRadius: 10)
                                .stroke(i == code.count && focused ? Theme.accent : Color.clear, lineWidth: 2))
                    }
                }
                .allowsHitTesting(false)
            }
            .onTapGesture { focused = true }
            #if DEBUG
            Text("开发模式验证码见服务器日志（默认 123456）")
                .font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
            #endif
            if let error { Text(error).font(Fonts.secondary).foregroundStyle(Theme.danger) }
            if loading { ProgressView() }
        } footer: {
            Button(countdown > 0 ? "重新发送（\(countdown)s）" : "重新发送") { resend() }
                .disabled(countdown > 0)
                .accessibilityIdentifier("resendButton")
        }
        .onAppear { focused = true }
        .onReceive(timer) { _ in if countdown > 0 { countdown -= 1 } }
    }

    private func submit() {
        loading = true; error = nil
        Task {
            do {
                try await HushCore.shared.verify(server: server.trimmingCharacters(in: .whitespaces),
                                                 email: email.trimmingCharacters(in: .whitespaces).lowercased(), code: code)
            } catch {
                self.error = describe(error)
                code = ""
            }
            loading = false
        }
    }

    private func resend() {
        countdown = 60
        Task {
            do { try await HushCore.shared.requestCode(server: server, email: email) } catch { self.error = describe(error) }
        }
    }
}

/// 登录各步骤的统一布局
struct AuthPage<Content: View, Footer: View>: View {
    let title: String
    let subtitle: String
    @ViewBuilder var content: Content
    @ViewBuilder var footer: Footer

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(title).font(.system(size: 26, weight: .bold)).foregroundStyle(Theme.textPrimary)
            Text(subtitle).font(Fonts.body).foregroundStyle(Theme.textSecondary)
            content.padding(.top, 8)
            Spacer()
            footer.frame(maxWidth: .infinity)
        }
        .padding(24)
        .background(Theme.background)
        .navigationBarTitleDisplayMode(.inline)
    }
}

extension View {
    func authField() -> some View {
        self.font(.system(size: 17))
            .padding(.horizontal, 14)
            .frame(height: 50)
            .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.surface))
    }
}
