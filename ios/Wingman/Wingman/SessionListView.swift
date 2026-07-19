import SwiftUI
import WingmanKit

/// Dashboard: all sessions on the paired machine, with live status.
struct SessionListView: View {
    @EnvironmentObject private var store: AppStore
    @State private var showNewSession = false

    var body: some View {
        NavigationStack {
            List {
                if store.sessions.isEmpty {
                    ContentUnavailableView(
                        "No sessions",
                        systemImage: "terminal",
                        description: Text("Start one with the + button or from your terminal.")
                    )
                } else {
                    ForEach(store.sessions) { session in
                        NavigationLink(value: session.id) {
                            SessionRow(session: session, hasPendingPermission: store.pendingPermissions[session.id] != nil)
                        }
                    }
                }
            }
            .navigationTitle("Wingman")
            .navigationDestination(for: String.self) { sessionID in
                SessionDetailView(sessionID: sessionID)
            }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    ConnectionBadge(state: store.connection)
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        showNewSession = true
                    } label: {
                        Image(systemName: "plus")
                    }
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Menu {
                        Button("Unpair device", role: .destructive) { store.unpair() }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                    }
                }
            }
            .refreshable { await store.refreshSessions() }
            .sheet(isPresented: $showNewSession) {
                NewSessionView()
            }
        }
    }
}

struct SessionRow: View {
    let session: SessionInfo
    let hasPendingPermission: Bool

    var body: some View {
        HStack(spacing: 12) {
            StatusDot(status: session.status)
            VStack(alignment: .leading, spacing: 2) {
                Text(directoryName)
                    .font(.headline)
                Text(session.status.replacingOccurrences(of: "_", with: " "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            if hasPendingPermission {
                Image(systemName: "exclamationmark.shield.fill")
                    .foregroundStyle(.orange)
            }
            Text(session.createdAt.formatted(date: .omitted, time: .shortened))
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 4)
    }

    private var directoryName: String {
        URL(fileURLWithPath: session.cwd).lastPathComponent
    }
}

struct StatusDot: View {
    let status: String

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 10, height: 10)
    }

    private var color: Color {
        switch status {
        case "running": return .blue
        case "awaiting_permission": return .orange
        case "idle": return .green
        case "error": return .red
        case "done": return .gray
        default: return .secondary.opacity(0.5)
        }
    }
}

struct ConnectionBadge: View {
    let state: ConnectionState

    var body: some View {
        switch state {
        case .connected(let via):
            Label(via, systemImage: "lock.fill")
                .font(.caption2)
                .foregroundStyle(.green)
        case .connecting:
            ProgressView().controlSize(.small)
        case .disconnected:
            Label("Offline", systemImage: "bolt.slash")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }
}

/// Start a session in a directory on the paired machine.
struct NewSessionView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.dismiss) private var dismiss
    @State private var cwd = ""
    @State private var prompt = ""
    @State private var isCreating = false

    var body: some View {
        NavigationStack {
            Form {
                Section("Working directory (absolute path on your machine)") {
                    TextField("/Users/you/projects/app", text: $cwd)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                        .font(.callout.monospaced())
                }
                Section("Initial prompt (optional)") {
                    TextField("Fix the failing CI on branch main…", text: $prompt, axis: .vertical)
                        .lineLimit(3...8)
                }
            }
            .navigationTitle("New session")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Start") {
                        isCreating = true
                        Task {
                            _ = await store.createSession(cwd: cwd, prompt: prompt)
                            isCreating = false
                            dismiss()
                        }
                    }
                    .disabled(cwd.isEmpty || isCreating)
                }
            }
        }
    }
}
