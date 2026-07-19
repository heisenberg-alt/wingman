import SwiftUI
import WingmanKit

/// Live transcript with prompt composer and permission approval sheet.
struct SessionDetailView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.surfaces) private var surfaces
    let sessionID: String
    @State private var promptText = ""
    @FocusState private var composerFocused: Bool

    private var session: SessionInfo? {
        store.sessions.first { $0.id == sessionID }
    }

    private var transcript: [TranscriptItem] {
        store.transcripts[sessionID] ?? []
    }

    private var isWorking: Bool {
        session?.status == "running"
    }

    var body: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 12) {
                        ForEach(transcript) { item in
                            TranscriptRow(item: item)
                        }
                        if isWorking {
                            WorkingIndicator()
                                .id("working")
                        }
                    }
                    .padding()
                }
                .background(surfaces.canvas)
                .onChange(of: transcript.count) { _, _ in
                    if let lastID = transcript.last?.id {
                        withAnimation(.snappy) { proxy.scrollTo(lastID, anchor: .bottom) }
                    }
                }
                .onTapGesture { composerFocused = false }
            }

            composer
        }
        .navigationTitle(session.map { URL(fileURLWithPath: $0.cwd).lastPathComponent } ?? "Session")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                if let session, session.status == "running" || session.status == "awaiting_permission" {
                    Button(role: .destructive) {
                        Task { await store.cancel(sessionID) }
                    } label: {
                        Image(systemName: "stop.circle.fill")
                            .foregroundStyle(.red)
                    }
                }
            }
            ToolbarItem(placement: .topBarTrailing) {
                if let session {
                    HStack(spacing: 6) {
                        StatusDot(status: session.status)
                        Text(session.status.replacingOccurrences(of: "_", with: " "))
                            .font(.caption.smallCaps())
                            .foregroundStyle(statusColor(session.status))
                    }
                }
            }
        }
        .task { await store.watch(sessionID) }
        .sheet(item: permissionBinding) { request in
            ApprovalSheet(sessionID: sessionID, request: request)
                .presentationDetents([.medium])
                .presentationCornerRadius(28)
                .interactiveDismissDisabled()
        }
        .sensoryFeedback(.impact(weight: .medium), trigger: transcript.count)
    }

    private var permissionBinding: Binding<PermissionRequest?> {
        Binding(
            get: { store.pendingPermissions[sessionID] },
            set: { if $0 == nil { store.pendingPermissions[sessionID] = nil } }
        )
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 10) {
            TextField("Message Copilot…", text: $promptText, axis: .vertical)
                .focused($composerFocused)
                .lineLimit(1...4)
                .padding(.horizontal, 14)
                .padding(.vertical, 9)
                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 20))

            Button {
                let text = promptText.trimmingCharacters(in: .whitespacesAndNewlines)
                promptText = ""
                Task { await store.sendPrompt(sessionID, text: text) }
            } label: {
                Image(systemName: "arrow.up")
                    .font(.body.bold())
                    .foregroundStyle(.white)
                    .frame(width: 34, height: 34)
                    .background(canSend ? Color.accentColor : Color.secondary.opacity(0.4), in: Circle())
            }
            .disabled(!canSend)
            .animation(.snappy, value: canSend)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(.bar)
    }

    private var canSend: Bool {
        !promptText.trimmingCharacters(in: .whitespaces).isEmpty
    }
}

struct TranscriptRow: View {
    @Environment(\.surfaces) private var surfaces
    let item: TranscriptItem

    var body: some View {
        switch item.kind {
        case .message:
            RichText(text: item.text)
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                .background(surfaces.card, in: RoundedRectangle(cornerRadius: 18))
                .frame(maxWidth: .infinity, alignment: .leading)

        case .thought:
            HStack(alignment: .top, spacing: 6) {
                Image(systemName: "bubble.and.pencil")
                    .font(.caption)
                Text(item.text)
                    .font(.callout)
                    .italic()
            }
            .foregroundStyle(.secondary)
            .padding(.horizontal, 6)

        case .tool:
            Label {
                Text(item.text)
                    .font(.footnote.monospaced())
                    .lineLimit(2)
            } icon: {
                Image(systemName: "wrench.and.screwdriver.fill")
                    .font(.footnote)
                    .foregroundStyle(.tint)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.tint.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))

        case .state:
            Text(item.text.replacingOccurrences(of: "_", with: " "))
                .font(.caption2.smallCaps())
                .foregroundStyle(.tertiary)
                .frame(maxWidth: .infinity)

        case .turnEnded:
            HStack {
                Rectangle().fill(.quaternary).frame(height: 1)
                Image(systemName: "checkmark.circle.fill")
                    .font(.caption)
                    .foregroundStyle(.green)
                Rectangle().fill(.quaternary).frame(height: 1)
            }
            .padding(.vertical, 2)
        }
    }
}

/// Three bouncing dots shown while the agent is working.
struct WorkingIndicator: View {
    @State private var phase = false

    var body: some View {
        HStack(spacing: 5) {
            ForEach(0..<3) { index in
                Circle()
                    .fill(.secondary)
                    .frame(width: 7, height: 7)
                    .offset(y: phase ? -4 : 2)
                    .animation(
                        .easeInOut(duration: 0.5)
                            .repeatForever(autoreverses: true)
                            .delay(Double(index) * 0.15),
                        value: phase
                    )
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
        .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 18))
        .onAppear { phase = true }
    }
}

/// The core Wingman interaction: approve or deny a tool call remotely.
struct ApprovalSheet: View {
    @EnvironmentObject private var store: AppStore
    let sessionID: String
    let request: PermissionRequest
    @State private var responding = false

    var body: some View {
        VStack(spacing: 0) {
            Capsule()
                .fill(.quaternary)
                .frame(width: 36, height: 5)
                .padding(.top, 10)

            Spacer()

            Image(systemName: "exclamationmark.shield.fill")
                .font(.system(size: 40))
                .foregroundStyle(.white)
                .frame(width: 76, height: 76)
                .background(.orange.gradient, in: RoundedRectangle(cornerRadius: 20))

            Text("Copilot requests permission")
                .font(.title3.bold())
                .padding(.top, 16)

            Text(request.title ?? "Tool call")
                .font(.callout.monospaced())
                .multilineTextAlignment(.center)
                .padding(.horizontal, 16)
                .padding(.vertical, 10)
                .frame(maxWidth: .infinity)
                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
                .padding(.top, 10)

            Spacer()

            VStack(spacing: 10) {
                ForEach(request.options) { option in
                    Button {
                        respond(option.optionId)
                    } label: {
                        Text(option.name)
                            .font(isAllow(option.kind) ? .headline : .body)
                            .frame(maxWidth: .infinity)
                            .padding(.vertical, 4)
                    }
                    .buttonStyle(.borderedProminent)
                    .buttonBorderShape(.capsule)
                    .tint(tint(for: option.kind))
                    .disabled(responding)
                }
            }
        }
        .padding(.horizontal, 24)
        .padding(.bottom, 16)
        .sensoryFeedback(.success, trigger: responding) { _, new in new }
    }

    private func respond(_ optionID: String) {
        responding = true
        Task {
            await store.approve(sessionID: sessionID, requestID: request.requestId, optionID: optionID)
        }
    }

    private func isAllow(_ kind: String) -> Bool {
        kind.hasPrefix("allow")
    }

    private func tint(for kind: String) -> Color {
        switch kind {
        case "allow_once": return .accentColor
        case "allow_always": return .indigo
        default: return .red
        }
    }
}
