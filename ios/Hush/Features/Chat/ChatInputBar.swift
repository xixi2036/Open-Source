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
                            .font(.system(size: 12, weight: .semibold)).foregroundStyle(Theme.accent)
                        Text(MessagePreview.text(q)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(1)
                    }
                    Spacer()
                    Button { model.quoting = nil } label: {
                        Image(systemName: "xmark.circle.fill").foregroundStyle(Theme.textSecondary)
                    }
                    .accessibilityIdentifier("cancelQuote")
                }
                .padding(.horizontal, 12).padding(.top, 8)
            }
            HStack(alignment: .bottom, spacing: 8) {
                Menu {
                    Button { showPhotos = true } label: { Label("相册", systemImage: "photo.on.rectangle") }
                        .accessibilityIdentifier("pickPhoto")
                    Button { showFiles = true } label: { Label("文件", systemImage: "doc") }
                        .accessibilityIdentifier("pickFile")
                } label: {
                    Image(systemName: "plus.circle.fill")
                        .font(.system(size: 28))
                        .foregroundStyle(Theme.accent)
                        .frame(width: 36, height: 38)
                }
                .accessibilityIdentifier("attachButton")

                MessageTextView(text: $model.draft, pendingCursor: $model.pendingCursor, placeholder: "发送消息")
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
                    .accessibilityIdentifier("sendButton")
                }
            }
            .padding(.horizontal, 8).padding(.vertical, 8)
        }
        .background(Theme.background)
        .overlay(alignment: .top) { Rectangle().fill(Theme.divider).frame(height: 0.5) }
        .photosPicker(isPresented: $showPhotos, selection: $photoItems, maxSelectionCount: 9, matching: .images)
        .onChange(of: photoItems) { _, items in
            guard !items.isEmpty else { return }
            photoItems = []
            Task {
                for item in items {
                    if let r = await PhotoLoader.load(item) { model.sendImage(url: r.url, width: r.width, height: r.height) }
                }
            }
        }
        .fileImporter(isPresented: $showFiles, allowedContentTypes: [.item]) { result in
            guard case let .success(url) = result else { return }
            let scoped = url.startAccessingSecurityScopedResource()
            defer { if scoped { url.stopAccessingSecurityScopedResource() } }
            let dir = AppPaths.outbox.appendingPathComponent(UUID().uuidString, isDirectory: true)
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let dst = dir.appendingPathComponent(url.lastPathComponent)
            do {
                try FileManager.default.copyItem(at: url, to: dst)
                model.sendFile(url: dst)
            } catch { core.toast = "无法读取文件" }
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
                Text("选择提醒的人").font(.system(size: 13, weight: .semibold)).foregroundStyle(Theme.textSecondary)
                Spacer()
                Button(action: onClose) { Image(systemName: "xmark").font(.system(size: 13, weight: .semibold)) }
                    .foregroundStyle(Theme.textSecondary)
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
