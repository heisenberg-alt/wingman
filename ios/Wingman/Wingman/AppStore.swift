import CryptoKit
import Foundation
import Network
import SwiftUI
import WingmanKit

/// Persistent pairing state: device identity plus how to reach the daemon.
struct PairingConfig: Codable {
    var privateKey: Data // Curve25519 raw representation
    var daemonPub: Data
    var lan: String?
    var relay: String?
    var room: String
    var deviceName: String
    var relayToken: String?

    var key: Curve25519.KeyAgreement.PrivateKey? {
        try? Curve25519.KeyAgreement.PrivateKey(rawRepresentation: privateKey)
    }
}

enum ConnectionState: Equatable {
    case disconnected
    case connecting
    case connected(via: String)
}

/// One rendered row of a session transcript.
struct TranscriptItem: Identifiable {
    enum Kind {
        case message
        case thought
        case tool
        case state
        case turnEnded
    }

    let id = UUID()
    let kind: Kind
    var text: String
}

@MainActor
final class AppStore: ObservableObject {
    @Published var config: PairingConfig?
    @Published var connection: ConnectionState = .disconnected
    @Published var sessions: [SessionInfo] = []
    @Published var transcripts: [String: [TranscriptItem]] = [:]
    @Published var pendingPermissions: [String: PermissionRequest] = [:] // sessionID → request
    @Published var lastError: String?
    /// Sessions with activity the user hasn't viewed yet.
    @Published var unread: Set<String> = []

    private var client: WingmanClient?
    private var pumpTask: Task<Void, Never>?
    private var refreshTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private let pathMonitor = NWPathMonitor()
    private var watched: Set<String> = []
    private var lastSeq: [String: UInt64] = [:]

    init() {
        config = Keychain.loadConfig()
        // Reconnect promptly when the network path changes (Wi-Fi ↔ hotspot
        // ↔ cellular), instead of waiting for the user to foreground the app.
        pathMonitor.pathUpdateHandler = { [weak self] path in
            guard path.status == .satisfied else { return }
            Task { @MainActor [weak self] in
                guard let self, self.connection == .disconnected else { return }
                await self.connect()
            }
        }
        pathMonitor.start(queue: .main)
    }

    deinit {
        pathMonitor.cancel()
    }
    // MARK: - Pairing

    func pair(payloadJSON: String, deviceName: String) async {
        do {
            let payload = try JSONDecoder.wingman.decode(PairingPayload.self, from: Data(payloadJSON.utf8))
            let key = Curve25519.KeyAgreement.PrivateKey()
            let newConfig = PairingConfig(
                privateKey: key.rawRepresentation,
                daemonPub: payload.pub,
                lan: payload.lan,
                relay: payload.relay,
                room: payload.room,
                deviceName: deviceName,
                relayToken: payload.relayToken
            )

            let (client, via) = try await dial(config: newConfig, key: key)
            try await client.pair(token: payload.token, deviceName: deviceName)

            Keychain.saveConfig(newConfig)
            config = newConfig
            adopt(client: client, via: via)
            await refreshSessions()
        } catch {
            lastError = "Pairing failed: \(error.localizedDescription)"
        }
    }

    func unpair() {
        pumpTask?.cancel()
        refreshTask?.cancel()
        reconnectTask?.cancel()
        Task { await client?.disconnect() }
        client = nil
        Keychain.deleteConfig()
        config = nil
        connection = .disconnected
        sessions = []
        transcripts = [:]
        pendingPermissions = [:]
        unread = []
        watched = []
        lastSeq = [:]
    }

    // MARK: - Connection

    func connect() async {
        guard let config, let key = config.key, client == nil else { return }
        connection = .connecting
        do {
            let (client, via) = try await dial(config: config, key: key)
            adopt(client: client, via: via)
            await refreshSessions()
            // Re-attach watches with resume after a reconnect.
            for id in watched {
                try? await client.watch(sessionID: id, fromSeq: lastSeq[id] ?? 0)
            }
        } catch {
            connection = .disconnected
            lastError = "Connection failed: \(error.localizedDescription)"
        }
    }

    /// Prefers the LAN path, falls back to the relay.
    private func dial(
        config: PairingConfig,
        key: Curve25519.KeyAgreement.PrivateKey
    ) async throws -> (WingmanClient, String) {
        if let lan = config.lan, !lan.isEmpty {
            let client = WingmanClient()
            do {
                try await client.connect(route: .lan(lan), staticKey: key, daemonPublicKey: config.daemonPub)
                return (client, "LAN")
            } catch {
                // fall through to relay
            }
        }
        guard let relay = config.relay, !relay.isEmpty else {
            throw ClientError.notConnected
        }
        let client = WingmanClient()
        try await client.connect(
            route: .relay(url: relay, room: config.room, token: config.relayToken),
            staticKey: key,
            daemonPublicKey: config.daemonPub
        )
        return (client, "relay")
    }

    private func adopt(client: WingmanClient, via: String) {
        self.client = client
        connection = .connected(via: via)
        reconnectTask?.cancel()
        pumpTask?.cancel()
        pumpTask = Task { [weak self] in
            for await envelope in await client.events {
                await MainActor.run { self?.handle(envelope) }
            }
            await MainActor.run {
                guard let self, self.client === client else { return }
                self.client = nil
                self.connection = .disconnected
                self.scheduleReconnect()
            }
        }
        // Until push notifications (Phase 4), keep the dashboard fresh by
        // polling the session list.
        refreshTask?.cancel()
        refreshTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(4))
                await self?.refreshSessions()
            }
        }
    }

    /// Retries the connection with exponential backoff until it succeeds or
    /// the device is unpaired. Network-path changes trigger immediate retries
    /// independently of this loop.
    private func scheduleReconnect() {
        reconnectTask?.cancel()
        reconnectTask = Task { [weak self] in
            var delay: Duration = .seconds(1)
            while !Task.isCancelled {
                try? await Task.sleep(for: delay)
                guard let self, self.config != nil else { return }
                if case .connected = self.connection { return }
                if self.connection != .disconnected { continue }
                await self.connect()
                if self.connection != .disconnected { return }
                delay = min(delay * 2, .seconds(30))
            }
        }
    }

    // MARK: - Commands

    func refreshSessions() async {
        guard let client else { return }
        do {
            sessions = try await client.listSessions()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func watch(_ sessionID: String) async {
        guard let client else { return }
        do {
            let fromSeq = lastSeq[sessionID] ?? 0
            if fromSeq == 0 {
                transcripts[sessionID] = []
            }
            try await client.watch(sessionID: sessionID, fromSeq: fromSeq)
            watched.insert(sessionID)
        } catch {
            lastError = error.localizedDescription
        }
    }

    func sendPrompt(_ sessionID: String, text: String) async {
        guard let client else { return }
        do {
            try await client.sendPrompt(sessionID: sessionID, text: text)
        } catch {
            lastError = error.localizedDescription
        }
    }

    func approve(sessionID: String, requestID: String, optionID: String) async {
        guard let client else { return }
        do {
            try await client.approve(sessionID: sessionID, requestID: requestID, optionID: optionID)
            pendingPermissions[sessionID] = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Answers a pending request from the dashboard with the first option of
    /// the given kind prefix ("allow" or "reject").
    func quickRespond(sessionID: String, allow: Bool) async {
        guard let request = pendingPermissions[sessionID] else { return }
        let prefix = allow ? "allow" : "reject"
        guard let option = request.options.first(where: { $0.kind.hasPrefix(prefix) }) else { return }
        await approve(sessionID: sessionID, requestID: request.requestId, optionID: option.optionId)
    }

    func cancel(_ sessionID: String) async {
        guard let client else { return }
        do {
            try await client.cancel(sessionID: sessionID)
        } catch {
            lastError = error.localizedDescription
        }
    }

    func removeSession(_ sessionID: String) async {
        guard let client else { return }
        do {
            try await client.removeSession(sessionID: sessionID)
            sessions.removeAll { $0.id == sessionID }
            transcripts[sessionID] = nil
            pendingPermissions[sessionID] = nil
            unread.remove(sessionID)
            watched.remove(sessionID)
            lastSeq[sessionID] = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    func listDirs() async -> [String] {
        guard let client else { return [] }
        return (try? await client.listDirs()) ?? []
    }

    /// Marks a session as viewed (called when its detail screen is open).
    func markRead(_ sessionID: String) {
        unread.remove(sessionID)
    }

    func createSession(cwd: String, prompt: String) async -> SessionInfo? {
        guard let client else { return nil }
        do {
            let info = try await client.createSession(cwd: cwd, prompt: prompt.isEmpty ? nil : prompt)
            await refreshSessions()
            return info
        } catch {
            lastError = error.localizedDescription
            return nil
        }
    }

    // MARK: - Event handling

    private func handle(_ envelope: Envelope) {
        guard let sessionID = envelope.sessionId else { return }
        if let seq = envelope.seq {
            lastSeq[sessionID] = max(lastSeq[sessionID] ?? 0, seq)
        }
        if envelope.type == Proto.evtTranscriptDelta || envelope.type == Proto.evtPermissionRequest {
            unread.insert(sessionID)
        }

        switch envelope.type {
        case Proto.evtSessionState:
            if let state = try? envelope.payload?.decode(SessionState.self) {
                if let index = sessions.firstIndex(where: { $0.id == sessionID }) {
                    sessions[index].status = state.status
                }
                append(sessionID, .init(kind: .state, text: state.status))
            }

        case Proto.evtTranscriptDelta:
            if let delta = try? envelope.payload?.decode(TranscriptDelta.self) {
                handleDelta(sessionID, delta)
            }

        case Proto.evtPermissionRequest:
            if let request = try? envelope.payload?.decode(PermissionRequest.self) {
                pendingPermissions[sessionID] = request
            }

        case Proto.evtPermissionResolved:
            if let resolved = try? envelope.payload?.decode(PermissionResolved.self) {
                if pendingPermissions[sessionID]?.requestId == resolved.requestId {
                    pendingPermissions[sessionID] = nil
                }
                append(sessionID, .init(kind: .state, text: "permission resolved by \(resolved.resolvedBy)"))
            }

        case Proto.evtTurnEnded:
            if let turn = try? envelope.payload?.decode(TurnEnded.self) {
                append(sessionID, .init(kind: .turnEnded, text: turn.stopReason))
            }

        default:
            break
        }
    }

    private func handleDelta(_ sessionID: String, _ delta: TranscriptDelta) {
        switch delta.kind {
        case "agent_message_chunk", "agent_thought_chunk":
            guard let text = delta.data["content"]?["text"]?.stringValue, !text.isEmpty else { return }
            let kind: TranscriptItem.Kind = delta.kind == "agent_thought_chunk" ? .thought : .message
            // Coalesce consecutive chunks of the same kind into one bubble.
            if var items = transcripts[sessionID], let last = items.indices.last, items[last].kind == kind {
                items[last].text += text
                transcripts[sessionID] = items
            } else {
                append(sessionID, .init(kind: kind, text: text))
            }

        case "tool_call":
            let title = delta.data["title"]?.stringValue ?? "tool call"
            append(sessionID, .init(kind: .tool, text: title))

        default:
            break // tool_call_update, plan, config updates: quiet for now
        }
    }

    private func append(_ sessionID: String, _ item: TranscriptItem) {
        transcripts[sessionID, default: []].append(item)
    }
}
