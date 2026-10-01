import SwiftUI
import PhotosUI
import UniformTypeIdentifiers

/// 输入栏：“+”（相册 / 文件）、多行文本框、发送按钮或按住录音
struct ChatInputBar: View {
    @Bindable var model: ChatModel
    var recorder: VoiceRecorder
    /// 群成员 user_id（群聊输入 @ 时选择提醒的人）
    var members: [String] = []
    @State private var showPhotos = false
    @State private var photoItems: [PhotosPickerItem] = []
    @State private var showFiles = false
    @State private var showCamera = false
    /// 正在准备待发送的媒体（压缩图片、拷贝视频）
    @State private var preparing = 0
    var core = HushCore.shared

    var body: some View {
        VStack(spacing: 0) {
            if model.isGroup, let q = model.mentionQuery {
                MentionPicker(candidates: model.mentionCandidates(members, query: q),
                              onPick: { model.pickMention($0) }, onClose: { model.cancelMention() })
            }
            if let q = model.quoting {
                HStack(spacing: 8) {
                    Rectangle().fill(Theme.accent).frame(width: 3, height: 34)
                    VStack(alignment: .leading, spacing: 2) {
                        Text("回复 \(q.isOutgoing ? "你" : core.contacts.displayName(q.senderId))")
                            .font(Fonts.sized(12, weight: .semibold)).foregroundStyle(Theme.accent)
                        Text(MessagePreview.text(q)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(1)
                    }
                    Spacer()
                    Button { model.quoting = nil } label: {
                        Image(systemName: "xmark.circle.fill").foregroundStyle(Theme.textSecondary)
                    }
                    .accessibilityLabel("取消回复")
                        .accessibilityIdentifier("cancelQuote")
                }
                .padding(.horizontal, 12).padding(.top, 8)
            }
            HStack(alignment: .bottom, spacing: 8) {
                Menu {
                    Button { showPhotos = true } label: { Label("相册（图片 / 视频）", systemImage: "photo.on.rectangle") }
                        .accessibilityIdentifier("pickPhoto")
                    if CameraPicker.isAvailable {
                        Button { openCamera() } label: { Label("拍照", systemImage: "camera") }
                            .accessibilityIdentifier("takePhoto")
                    }
                    Button { showFiles = true } label: { Label("文件", systemImage: "doc") }
                        .accessibilityIdentifier("pickFile")
                } label: {
                    if preparing > 0 {
                        ProgressView().frame(width: 36, height: 38)
                    } else {
                        Image(systemName: "plus.circle.fill")
                            .font(.system(size: 28))
                            .foregroundStyle(Theme.accent)
                            .frame(width: 36, height: 38)
                    }
                }
                .accessibilityLabel(preparing > 0 ? "正在处理附件" : "发送图片、视频或文件")
                .accessibilityIdentifier("attachButton")

                MessageTextView(text: $model.draft, pendingCursor: $model.pendingCursor,
                                cursorToEndOnFocus: $model.cursorToEndOnFocus, placeholder: "发送消息")
                    .overlay(alignment: .leading) {
                        if model.draft.isEmpty {
                            Text("发送消息").font(Fonts.body).foregroundStyle(Theme.textSecondary.opacity(0.7))
                                .allowsHitTesting(false).accessibilityHidden(true)
                        }
                    }
                    .padding(.horizontal, 14).padding(.vertical, 9)
                    .background(RoundedRectangle(cornerRadius: 20).fill(Theme.surface))
                    .onChange(of: model.draft) { _, _ in model.draftChanged() }

                if model.draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                    MicButton(recorder: recorder) { url, ms in model.sendVoice(url: url, durationMs: ms) }
                } else {
                    Button { model.sendText() } label: {
                        Image(systemName: "arrow.up.circle.fill")
                            .font(.system(size: 32))
                            .foregroundStyle(Theme.accent)
                            .frame(width: 38, height: 38)
                    }
                    .accessibilityLabel("发送")
                    .accessibilityIdentifier("sendButton")
                }
            }
            .padding(.horizontal, 8).padding(.vertical, 8)
        }
        .background(Theme.background)
        .overlay(alignment: .top) { Rectangle().fill(Theme.divider).frame(height: 0.5) }
        // 一次最多选 9 项，图片和视频都可以；按选择顺序逐条发送
        .photosPicker(isPresented: $showPhotos, selection: $photoItems, maxSelectionCount: 9,
                      selectionBehavior: .ordered, matching: .any(of: [.images, .videos]))
        .onChange(of: photoItems) { _, items in
            guard !items.isEmpty else { return }
            photoItems = []
            sendPicked(items)
        }
        .fullScreenCover(isPresented: $showCamera) {
            CameraPicker { url in
                guard let url else { return }
                preparing += 1
                Task {
                    defer { preparing -= 1 }
                    let media = await PhotoLoader.prepareImage(fileURL: url)
                    try? FileManager.default.removeItem(at: url)
                    if let media { model.sendMedia([media]) } else { core.toast = "无法处理这张照片" }
                }
            }
            .ignoresSafeArea()
        }
        .fileImporter(isPresented: $showFiles, allowedContentTypes: [.item]) { result in
            guard case let .success(url) = result else { return }
            // C4：拷贝放到后台线程（大文件会卡住主线程）；安全作用域访问保持到拷贝结束
            let scoped = url.startAccessingSecurityScopedResource()
            preparing += 1
            Task {
                let dst = await Task.detached(priority: .userInitiated) { () -> URL? in
                    defer { if scoped { url.stopAccessingSecurityScopedResource() } }
                    let dir = AppPaths.outbox.appendingPathComponent(UUID().uuidString, isDirectory: true)
                    try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                    let dst = dir.appendingPathComponent(url.lastPathComponent)
                    do { try FileManager.default.copyItem(at: url, to: dst) } catch { return nil }
                    return dst
                }.value
                preparing -= 1
                if let dst { model.sendFile(url: dst) } else { core.toast = "无法读取文件" }
            }
        }
    }

    /// 把相册里选中的项逐个准备好（后台压缩 / 拷贝）再按顺序发送；读不了或超过大小上限的给出提示
    private func sendPicked(_ items: [PhotosPickerItem]) {
        preparing += 1
        Task {
            defer { preparing -= 1 }
            var ready: [PreparedMedia] = []
            var unreadable = 0
            for item in items {
                switch await PhotoLoader.prepare(item) {
                case let .success(m): ready.append(m)
                case let .failure(.tooLarge(size)): core.toast = VideoMeta.tooLargeText(size)
                case .failure: unreadable += 1
                }
            }
            if unreadable > 0 { core.toast = "有 \(unreadable) 项无法读取" }
            if !ready.isEmpty { model.sendMedia(ready) }
        }
    }

    private func openCamera() {
        switch CameraPicker.authorization() {
        case .denied:
            core.toast = "请在系统「设置 › Hush」中允许使用相机"
        case .undetermined:
            Task { if await CameraPicker.requestAccess() { showCamera = true } else { core.toast = "没有相机权限，无法拍照" } }
        case .granted:
            showCamera = true
        }
    }
}

/// 按住录音，上滑取消，松开发送
struct MicButton: View {
    var recorder: VoiceRecorder
    var onFinish: (URL, UInt64) -> Void
    @GestureState private var pressing = false

    var body: some View {
        Image(systemName: recorder.isRecording ? "mic.circle.fill" : "mic.fill")
            .font(.system(size: recorder.isRecording ? 34 : 22))
            .foregroundStyle(recorder.isRecording ? Theme.danger : Theme.accent)
            .frame(width: 38, height: 38)
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 0)
                    .updating($pressing) { _, st, _ in st = true }
                    .onChanged { v in
                        recorder.onAutoFinish = onFinish
                        recorder.pressChanged(translationY: v.translation.height)
                    }
                    .onEnded { _ in
                        if let (url, ms) = recorder.pressEnded() { onFinish(url, ms) }
                    }
            )
            .onChange(of: pressing) { _, isPressing in
                // 手势被取消（如权限弹窗）时不会调用 onEnded：下一轮检查仍未结束就丢弃本次录音
                guard !isPressing else { return }
                DispatchQueue.main.async { recorder.pressReset() }
            }
            .accessibilityLabel("按住说话")
            .accessibilityIdentifier("micButton")
    }
}

/// 录音中浮层
struct RecordingOverlay: View {
    var recorder: VoiceRecorder
    var body: some View {
        VStack(spacing: 12) {
            Image(systemName: recorder.willCancel ? "arrow.uturn.backward.circle.fill" : "waveform")
                .font(.system(size: 40))
                .foregroundStyle(.white)
                .symbolEffect(.variableColor.iterative, isActive: !recorder.willCancel)
            Text(Fmt.duration(Int(recorder.elapsed)))
                .font(.system(size: 20, weight: .semibold, design: .monospaced)).foregroundStyle(.white)
            Text(recorder.willCancel ? "松开手指，取消发送" : "上滑取消，松开发送")
                .font(Fonts.secondary).foregroundStyle(.white)
                .padding(.horizontal, 8).padding(.vertical, 3)
                .background(RoundedRectangle(cornerRadius: 4).fill(recorder.willCancel ? Theme.danger : Color.clear))
        }
        .frame(width: 170, height: 170)
        .background(RoundedRectangle(cornerRadius: 16).fill(Color.black.opacity(0.7)))
        .accessibilityIdentifier("recordingOverlay")
    }
}

/// 群聊输入「@」后在输入栏上方弹出的成员列表（继续输入可筛选）
struct MentionPicker: View {
    let candidates: [String]
    var onPick: (String) -> Void
    var onClose: () -> Void
    var core = HushCore.shared

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("选择提醒的人").font(Fonts.sized(13, weight: .semibold)).foregroundStyle(Theme.textSecondary)
                Spacer()
                Button(action: onClose) { Image(systemName: "xmark").font(.system(size: 13, weight: .semibold)) }
                    .foregroundStyle(Theme.textSecondary)
                    .accessibilityLabel("关闭")
                        .accessibilityIdentifier("mentionPickerClose")
            }
            .padding(.horizontal, 14).padding(.vertical, 8)
            if candidates.isEmpty {
                Text("没有匹配的群成员").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    .frame(maxWidth: .infinity).padding(.vertical, 12)
            } else {
                ScrollView {
                    LazyVStack(spacing: 0) {
                        ForEach(candidates, id: \.self) { uid in
                            let name = core.contacts.displayName(uid)
                            Button { onPick(uid) } label: {
                                HStack(spacing: 10) {
                                    UserAvatar(userId: uid, size: 32)
                                    Text(name).foregroundStyle(Theme.textPrimary)
                                    Spacer()
                                }
                                .padding(.horizontal, 14).padding(.vertical, 6)
                                .contentShape(Rectangle())
                            }
                            .accessibilityIdentifier("mention_\(name)")
                        }
                    }
                }
                .frame(maxHeight: min(CGFloat(candidates.count) * 44, 200))
            }
        }
        .background(Theme.surface)
        .overlay(alignment: .top) { Rectangle().fill(Theme.divider).frame(height: 0.5) }
        .accessibilityIdentifier("mentionPicker")
    }
}
