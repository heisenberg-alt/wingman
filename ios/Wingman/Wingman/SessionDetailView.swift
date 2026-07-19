import SwiftUI
import WingmanKit

/// Live transcript with prompt composer and permission approval sheet.
struct SessionDetailView: View {
    @EnvironmentObject private var store: AppStore
    let sessionID: String
    @State private var promptText = ""

    private var session: SessionInfo? {
        store.sessions.first { $0.id == sessionID }
    }

    private var transcript: [TranscriptItem] {
        store.transcripts[sessionID] ?? []
    }

    var body: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        ForEach(transcript) { item in
                            TranscriptRow(item: item)
                        }
                    }
                    .padding()
                }
                .onChange(of: transcript.count) { _, _ in
                    if let lastID = transcript.last?.id {
                        withAnimation { proxy.scrollTo(lastID, anchor: .bottom) }
                    }
                }
            }

            composer
        }
        .navigationTitle(session.map { URL(fileURLWithPath: $0.cwd).lastPathComponent } ?? "Session")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                if let session {
                    StatusDot(status: session.status)
                }
            }
        }
        .task { await store.watch(sessionID) }
        .sheet(item: permissionBinding) { request in
            ApprovalSheet(sessionID: sessionID, request: request)
                .presentationDetents([.medium])
                .interactiveDismissDisabled()
        }
    }

    private var permissionBinding: Binding<PermissionRequest?> {
        Binding(
            get: { store.pendingPermissions[sessionID] },
            set: { if $0 == nil { store.pendingPermissions[sessionID] = nil } }
        )
    }

    private var composer: some View {
        HStack(spacing: 8) {
            TextField("Send a prompt…", text: $promptText, axis: .vertical)
                .textFieldStyle(.roundedBorder)
                .lineLimit(1...4)
            Button {
                let text = promptText
                promptText = ""
                Task { await store.sendPrompt(sessionID, text: text) }
            } label: {
                Image(systemName: "arrow.up.circle.fill")
                    .font(.title2)
            }
            .disabled(promptText.trimmingCharacters(in: .whitespaces).isEmpty)
        }
        .padding(12)
        .background(.bar)
    }
}

struct TranscriptRow: View {
    let item: TranscriptItem

    var body: some View {
        switch item.kind {
        case .message:
            Text(item.text)
                .textSelection(.enabled)
                .padding(12)
                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))

        case .thought:
            Text(item.text)
                .font(.callout)
                .foregroundStyle(.secondary)
                .italic()
                .padding(.horizontal, 12)

        case .tool:
            Label(item.text, systemImage: "wrench.and.screwdriver")
                .font(.callout.monospaced())
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))

        case .state:
            Text(item.text.replacingOccurrences(of: "_", with: " "))
                .font(.caption2.smallCaps())
                .foregroundStyle(.tertiary)
                .frame(maxWidth: .infinity)

        case .turnEnded:
            Divider()
        }
    }
}

/// The core Wingman interaction: approve or deny a tool call remotely.
struct ApprovalSheet: View {
    @EnvironmentObject private var store: AppStore
    let sessionID: String
    let request: PermissionRequest

    var body: some View {
        VStack(spacing: 20) {
            Image(systemName: "exclamationmark.shield.fill")
                .font(.system(size: 44))
                .foregroundStyle(.orange)

            Text("Copilot requests permission")
                .font(.headline)
            Text(request.title ?? "Tool call")
                .font(.title3.monospaced())
                .multilineTextAlignment(.center)

            VStack(spacing: 10) {
                ForEach(request.options) { option in
                    Button {
                        Task {
                            await store.approve(
                                sessionID: sessionID,
                                requestID: request.requestId,
                                optionID: option.optionId
                            )
                        }
                    } label: {
                        Text(option.name)
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(tint(for: option.kind))
                    .controlSize(.large)
                }
            }
        }
        .padding(24)
    }

    private func tint(for kind: String) -> Color {
        switch kind {
        case "allow_once": return .blue
        case "allow_always": return .indigo
        default: return .red
        }
    }
}
