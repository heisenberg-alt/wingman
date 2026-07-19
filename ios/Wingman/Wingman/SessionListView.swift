import SwiftUI
import WingmanKit

/// Dashboard filter.
enum SessionFilter: String, CaseIterable, Identifiable {
    case all = "All"
    case active = "Active"
    case approvals = "Approvals"
    case finished = "Finished"

    var id: String { rawValue }

    func matches(_ session: SessionInfo, pending: Bool) -> Bool {
        switch self {
        case .all: return true
        case .active: return session.status == "running" || session.status == "awaiting_permission" || session.status == "idle"
        case .approvals: return pending
        case .finished: return session.status == "done" || session.status == "error"
        }
    }
}

/// Dashboard: all sessions on the paired machine, with live status.
struct SessionListView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.surfaces) private var surfaces
    @State private var showNewSession = false
    @State private var showSettings = false
    @State private var path = NavigationPath()
    @State private var filter: SessionFilter = .all

    private var filteredSessions: [SessionInfo] {
        store.sessions.filter {
            filter.matches($0, pending: store.pendingPermissions[$0.id] != nil)
        }
    }

    var body: some View {
        NavigationStack(path: $path) {
            Group {
                if store.sessions.isEmpty {
                    emptyState
                } else {
                    sessionList
                }
            }
            .navigationTitle("Wingman")
            .navigationDestination(for: String.self) { sessionID in
                SessionDetailView(sessionID: sessionID)
            }
            .safeAreaInset(edge: .top) {
                if store.connection == .disconnected {
                    ReconnectBanner()
                }
            }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    ConnectionBadge(state: store.connection)
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        showSettings = true
                    } label: {
                        Image(systemName: "gearshape")
                    }
                }
            }
            .safeAreaInset(edge: .bottom) {
                newSessionButton
            }
            .refreshable { await store.refreshSessions() }
            .sheet(isPresented: $showNewSession) {
                NewSessionView()
                    .presentationDetents([.medium])
            }
            .sheet(isPresented: $showSettings) {
                SettingsView()
            }
            .onChange(of: store.sessions.count) { _, _ in
                #if DEBUG
                // UI-test hook: auto-navigate into the newest session.
                if ProcessInfo.processInfo.environment["WINGMAN_AUTO_OPEN"] != nil,
                   path.isEmpty, let first = store.sessions.first {
                    path.append(first.id)
                }
                #endif
            }
            .sensoryFeedback(.warning, trigger: store.pendingPermissions.count) { old, new in
                new > old // buzz when a new approval arrives
            }
        }
    }

    private var sessionList: some View {
        ScrollView {
            LazyVStack(spacing: 12) {
                Picker("Filter", selection: $filter) {
                    ForEach(SessionFilter.allCases) { option in
                        Text(option.rawValue).tag(option)
                    }
                }
                .pickerStyle(.segmented)
                .padding(.bottom, 4)

                ForEach(filteredSessions) { session in
                    NavigationLink(value: session.id) {
                        SessionCard(
                            session: session,
                            pendingRequest: store.pendingPermissions[session.id],
                            isUnread: store.unread.contains(session.id),
                            onQuickRespond: { allow in
                                Task { await store.quickRespond(sessionID: session.id, allow: allow) }
                            }
                        )
                    }
                    .buttonStyle(.plain)
                    .contextMenu {
                        if session.status == "done" || session.status == "error" {
                            Button("Remove session", systemImage: "trash", role: .destructive) {
                                Task { await store.removeSession(session.id) }
                            }
                        }
                    }
                }
            }
            .padding(.horizontal)
            .padding(.top, 4)
        }
        .background(surfaces.canvas)
        .animation(.snappy, value: store.sessions)
    }

    private var emptyState: some View {
        ContentUnavailableView {
            Label("No sessions", systemImage: "terminal")
        } description: {
            Text("Start one below, or run `copilot` on your machine.")
        }
    }

    private var newSessionButton: some View {
        Button {
            showNewSession = true
        } label: {
            Label("New session", systemImage: "plus")
                .font(.headline)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 6)
        }
        .buttonStyle(.borderedProminent)
        .buttonBorderShape(.capsule)
        .padding(.horizontal)
        .padding(.bottom, 8)
        .background(.ultraThinMaterial.opacity(store.sessions.isEmpty ? 0 : 1))
    }
}

/// One session, rendered as a card. Pending permission requests surface
/// inline approve/deny actions for the fastest possible response.
struct SessionCard: View {
    @Environment(\.surfaces) private var surfaces
    let session: SessionInfo
    let pendingRequest: PermissionRequest?
    var isUnread = false
    var onQuickRespond: (Bool) -> Void = { _ in }

    private var hasPendingPermission: Bool { pendingRequest != nil }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 14) {
                StatusDot(status: session.status)
                    .frame(width: 14, height: 14)

                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 6) {
                        Text(directoryName)
                            .font(.headline)
                            .lineLimit(1)
                        if isUnread {
                            Circle()
                                .fill(.tint)
                                .frame(width: 8, height: 8)
                        }
                    }
                    Text(session.cwd)
                        .font(.caption.monospaced())
                        .foregroundStyle(.tertiary)
                        .lineLimit(1)
                        .truncationMode(.head)
                }

                Spacer(minLength: 8)

                VStack(alignment: .trailing, spacing: 4) {
                    Text(statusLabel)
                        .font(.caption2.smallCaps())
                        .foregroundStyle(statusColor(session.status))
                    Text(session.createdAt, format: .relative(presentation: .named))
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                }
            }

            if let request = pendingRequest {
                Divider()
                    .padding(.vertical, 10)
                HStack(spacing: 10) {
                    Label(request.title ?? "Permission requested", systemImage: "exclamationmark.shield.fill")
                        .font(.caption.bold())
                        .foregroundStyle(.orange)
                        .lineLimit(1)
                    Spacer()
                    Button("Deny") { onQuickRespond(false) }
                        .buttonStyle(.bordered)
                        .buttonBorderShape(.capsule)
                        .controlSize(.small)
                        .tint(.red)
                    Button("Allow") { onQuickRespond(true) }
                        .buttonStyle(.borderedProminent)
                        .buttonBorderShape(.capsule)
                        .controlSize(.small)
                }
            }
        }
        .padding(14)
        .background(surfaces.card, in: RoundedRectangle(cornerRadius: Brand.cardCornerRadius))
        .overlay {
            if hasPendingPermission {
                RoundedRectangle(cornerRadius: Brand.cardCornerRadius)
                    .strokeBorder(.orange.opacity(0.5), lineWidth: 1.5)
            }
        }
    }

    private var directoryName: String {
        URL(fileURLWithPath: session.cwd).lastPathComponent
    }

    private var statusLabel: String {
        session.status.replacingOccurrences(of: "_", with: " ")
    }
}

func statusColor(_ status: String) -> Color {
    switch status {
    case "running": return .blue
    case "awaiting_permission": return .orange
    case "idle": return .green
    case "error": return .red
    case "done": return .secondary
    default: return .secondary
    }
}

/// Status indicator that pulses while the session is active.
struct StatusDot: View {
    let status: String
    @State private var pulsing = false

    private var isActive: Bool {
        status == "running" || status == "awaiting_permission"
    }

    var body: some View {
        ZStack {
            if isActive {
                Circle()
                    .fill(statusColor(status).opacity(0.35))
                    .scaleEffect(pulsing ? 1.9 : 1.0)
                    .opacity(pulsing ? 0 : 1)
                    .animation(.easeOut(duration: 1.2).repeatForever(autoreverses: false), value: pulsing)
            }
            Circle()
                .fill(statusColor(status))
                .frame(width: 10, height: 10)
        }
        .onAppear { pulsing = isActive }
        .onChange(of: status) { _, _ in pulsing = isActive }
    }
}

/// Banner shown while the connection is down; retries happen automatically.
struct ReconnectBanner: View {
    @EnvironmentObject private var store: AppStore

    var body: some View {
        HStack(spacing: 8) {
            ProgressView()
                .controlSize(.small)
            Text("Reconnecting…")
                .font(.footnote.bold())
            Spacer()
            Button("Retry now") {
                Task { await store.connect() }
            }
            .font(.footnote.bold())
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 8)
        .background(.orange.opacity(0.15))
        .overlay(alignment: .bottom) { Divider() }
    }
}

struct ConnectionBadge: View {
    let state: ConnectionState

    var body: some View {
        switch state {
        case .connected(let via):
            Label(via, systemImage: "lock.fill")
                .font(.caption.bold())
                .foregroundStyle(.green)
                .padding(.horizontal, 8)
                .padding(.vertical, 3)
                .background(.green.opacity(0.14), in: Capsule())
        case .connecting:
            ProgressView().controlSize(.small)
        case .disconnected:
            Label("Offline", systemImage: "bolt.slash.fill")
                .font(.caption.bold())
                .foregroundStyle(.secondary)
                .padding(.horizontal, 8)
                .padding(.vertical, 3)
                .background(.secondary.opacity(0.12), in: Capsule())
        }
    }
}

/// Start a session in a directory on the paired machine. Recent directories
/// are offered as tappable choices; typing is the fallback.
struct NewSessionView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.dismiss) private var dismiss
    @State private var cwd = ""
    @State private var prompt = ""
    @State private var isCreating = false
    @State private var recentDirs: [String] = []

    var body: some View {
        NavigationStack {
            Form {
                if !recentDirs.isEmpty {
                    Section("Recent directories") {
                        ForEach(recentDirs, id: \.self) { dir in
                            Button {
                                cwd = dir
                            } label: {
                                HStack {
                                    Image(systemName: "clock.arrow.circlepath")
                                        .foregroundStyle(.tint)
                                    VStack(alignment: .leading, spacing: 1) {
                                        Text(URL(fileURLWithPath: dir).lastPathComponent)
                                            .font(.callout.bold())
                                            .foregroundStyle(.primary)
                                        Text(dir)
                                            .font(.caption2.monospaced())
                                            .foregroundStyle(.tertiary)
                                            .lineLimit(1)
                                            .truncationMode(.head)
                                    }
                                    Spacer()
                                    if cwd == dir {
                                        Image(systemName: "checkmark")
                                            .foregroundStyle(.tint)
                                    }
                                }
                            }
                        }
                    }
                }
                Section {
                    TextField("/Users/you/projects/app", text: $cwd)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                        .font(.callout.monospaced())
                } header: {
                    Text(recentDirs.isEmpty ? "Working directory" : "Or type a path")
                } footer: {
                    Text("Absolute path on your paired machine.")
                }
                Section("Initial prompt (optional)") {
                    TextField("Fix the failing CI on branch main…", text: $prompt, axis: .vertical)
                        .lineLimit(3...8)
                }
            }
            .task {
                recentDirs = await store.listDirs()
                if cwd.isEmpty, let first = recentDirs.first {
                    cwd = first
                }
            }
            .navigationTitle("New session")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    if isCreating {
                        ProgressView()
                    } else {
                        Button("Start") {
                            isCreating = true
                            Task {
                                _ = await store.createSession(cwd: cwd, prompt: prompt)
                                isCreating = false
                                dismiss()
                            }
                        }
                        .disabled(cwd.isEmpty)
                    }
                }
            }
        }
    }
}
