import SwiftUI
import PhotosUI

/// 发布动态：文字、最多 9 张图（压缩到长边 ≤ 2048、JPEG 0.8）、可见范围
struct ComposeMomentView: View {
    var onPosted: (Moment) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""
    @State private var picks: [PhotosPickerItem] = []
    @State private var images: [URL] = []
    @State private var mode: VisibilityMode = .all
    @State private var selected: Set<String> = []
    @State private var posting = false
    var core = HushCore.shared

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("这一刻的想法…", text: $text, axis: .vertical)
                        .lineLimit(4...10)
                        .accessibilityIdentifier("momentText")
                    LazyVGrid(columns: Array(repeating: GridItem(.fixed(90), spacing: 6), count: 3), alignment: .leading, spacing: 6) {
                        ForEach(images, id: \.self) { url in
                            ZStack(alignment: .topTrailing) {
                                LocalImage(path: url.path, maxPixel: 300) { Theme.surface }
                                    .frame(width: 90, height: 90).clipped()
                                Button { images.removeAll { $0 == url } } label: {
                                    Image(systemName: "xmark.circle.fill").foregroundStyle(.white, .black.opacity(0.5))
                                }
                                .buttonStyle(.plain)
                                .padding(3)
                            }
                        }
                        if images.count < 9 {
                            PhotosPicker(selection: $picks, maxSelectionCount: 9 - images.count, matching: .images) {
                                RoundedRectangle(cornerRadius: 4).fill(Theme.surface)
                                    .frame(width: 90, height: 90)
                                    .overlay(Image(systemName: "plus").font(.system(size: 28)).foregroundStyle(Theme.textSecondary))
                            }
                            .buttonStyle(.plain)
                            .accessibilityIdentifier("addMomentPhotos")
                        }
                    }
                    .padding(.vertical, 6)
                }
                Section {
                    NavigationLink {
                        VisibilityPicker(mode: $mode, selected: $selected)
                    } label: {
                        HStack {
                            Label("谁可以看", systemImage: "person.2")
                            Spacer()
                            Text(visibilityLabel).foregroundStyle(Theme.textSecondary)
                        }
                    }
                    .accessibilityIdentifier("visibilityEntry")
                }
            }
            .navigationTitle("发表动态")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(posting ? "发表中…" : "发表") { post() }
                        .disabled(posting || (text.trimmingCharacters(in: .whitespaces).isEmpty && images.isEmpty))
                        .accessibilityIdentifier("postMoment")
                }
            }
            .onChange(of: picks) { _, items in
                guard !items.isEmpty else { return }
                picks = []
                Task {
                    for item in items {
                        if let r = await PhotoLoader.load(item), images.count < 9 { images.append(r.url) }
                    }
                }
            }
        }
    }

    private var visibilityLabel: String {
        switch mode {
        case .all: return "公开给所有好友"
        case .include: return "部分可见（\(selected.count)）"
        case .exclude: return "不给谁看（\(selected.count)）"
        }
    }

    private func post() {
        posting = true
        let vis = Visibility(mode: mode, userIds: mode == .all ? [] : Array(selected))
        Task {
            do {
                let m = try await core.moments.post(text: text.trimmingCharacters(in: .whitespacesAndNewlines),
                                                         imagePaths: images.map(\.path), visibility: vis)
                images.forEach { AppPaths.discardOutbox($0.path) }
                onPosted(m)
                dismiss()
            } catch { core.toast = describe(error) }
            posting = false
        }
    }
}

struct VisibilityPicker: View {
    @Binding var mode: VisibilityMode
    @Binding var selected: Set<String>
    var core = HushCore.shared

    var body: some View {
        List {
            Section {
                option(.all, "公开给所有好友", "所有好友可见")
                option(.include, "部分可见", "选中的好友可见")
                option(.exclude, "不给谁看", "选中的好友不可见")
            }
            if mode != .all {
                Section("选择好友") { MultiFriendPicker(candidates: core.contacts.friends, selected: $selected) }
            }
        }
        .navigationTitle("谁可以看")
        .navigationBarTitleDisplayMode(.inline)
    }

    private func option(_ m: VisibilityMode, _ title: String, _ sub: String) -> some View {
        Button { mode = m } label: {
            HStack {
                VStack(alignment: .leading) {
                    Text(title).foregroundStyle(Theme.textPrimary)
                    Text(sub).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                }
                Spacer()
                if mode == m { Image(systemName: "checkmark").foregroundStyle(Theme.accent) }
            }
        }
    }
}
