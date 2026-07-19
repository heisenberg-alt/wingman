// WingmanClient: the phone side of the Wingman wire protocol. Connects over
// a WebSocket (LAN or relay), runs the Noise XX handshake with the daemon key
// pinned, and multiplexes command replies and session events.
import CryptoKit
import Foundation

public enum ClientError: Error, LocalizedError {
    case notConnected
    case commandFailed(String)
    case connectionClosed
    case badReply

    public var errorDescription: String? {
        switch self {
        case .notConnected: return "Not connected to the daemon"
        case .commandFailed(let message): return message
        case .connectionClosed: return "Connection closed"
        case .badReply: return "Malformed reply from daemon"
        }
    }
}

/// How to reach the daemon.
public enum Route: Sendable {
    case lan(String)               // host:port of the external listener
    case relay(url: String, room: String, token: String?)

    var webSocketURL: URL? {
        switch self {
        case .lan(let address):
            return URL(string: "ws://\(address)/ws")
        case .relay(let url, let room, let token):
            var components = URLComponents(string: url)
            components?.path = "/v1/join"
            var items = [URLQueryItem(name: "room", value: room)]
            if let token, !token.isEmpty {
                items.append(URLQueryItem(name: "token", value: token))
            }
            components?.queryItems = items
            return components?.url
        }
    }
}

public actor WingmanClient {
    private var task: URLSessionWebSocketTask?
    private var session: NoiseSession?
    private var nextID = 0
    private var pendingReplies: [String: CheckedContinuation<WireResult, Error>] = [:]
    private let eventContinuation: AsyncStream<Envelope>.Continuation

    /// Session events (state, transcript, permissions), in arrival order.
    public let events: AsyncStream<Envelope>

    public init() {
        (events, eventContinuation) = AsyncStream.makeStream(of: Envelope.self)
    }

    /// Connects and performs the Noise handshake, pinning the daemon key.
    public func connect(
        route: Route,
        staticKey: Curve25519.KeyAgreement.PrivateKey,
        daemonPublicKey: Data
    ) async throws {
        guard let url = route.webSocketURL else { throw ClientError.notConnected }

        let task = URLSession.shared.webSocketTask(with: url)
        task.maximumMessageSize = 16 << 20
        task.resume()
        self.task = task

        var handshake = NoiseHandshakeInitiator(staticKey: staticKey)
        try await send(raw: handshake.writeMessage1())
        try handshake.readMessage2(await receiveRaw())
        let (message3, noiseSession) = try handshake.writeMessage3()
        try await send(raw: message3)

        guard noiseSession.remoteStaticKey == daemonPublicKey else {
            task.cancel(with: .policyViolation, reason: nil)
            throw NoiseError.peerKeyMismatch
        }
        session = noiseSession

        Task { await self.readLoop() }
    }

    public func disconnect() {
        task?.cancel(with: .normalClosure, reason: nil)
        task = nil
        session = nil
        for (_, continuation) in pendingReplies {
            continuation.resume(throwing: ClientError.connectionClosed)
        }
        pendingReplies.removeAll()
    }

    // MARK: - Commands

    public func pair(token: String, deviceName: String) async throws {
        _ = try await call(Proto.cmdPairRequest, payload: PairRequest(token: token, deviceName: deviceName))
    }

    public func listSessions() async throws -> [SessionInfo] {
        let result = try await call(Proto.cmdSessionList)
        guard let data = result.data else { throw ClientError.badReply }
        return try data.decode(SessionList.self).sessions
    }

    public func createSession(cwd: String, prompt: String?) async throws -> SessionInfo {
        let result = try await call(Proto.cmdSessionCreate, payload: SessionCreate(cwd: cwd, prompt: prompt))
        guard let data = result.data else { throw ClientError.badReply }
        return try data.decode(SessionInfo.self)
    }

    public func sendPrompt(sessionID: String, text: String) async throws {
        _ = try await call(Proto.cmdSessionPrompt, sessionID: sessionID, payload: SessionPrompt(text: text))
    }

    public func approve(sessionID: String, requestID: String, optionID: String) async throws {
        _ = try await call(
            Proto.cmdSessionApprove,
            sessionID: sessionID,
            payload: SessionApprove(requestId: requestID, optionId: optionID)
        )
    }

    public func cancel(sessionID: String) async throws {
        _ = try await call(Proto.cmdSessionCancel, sessionID: sessionID)
    }

    public func watch(sessionID: String, fromSeq: UInt64) async throws {
        _ = try await call(Proto.cmdSessionWatch, sessionID: sessionID, payload: SessionWatch(fromSeq: fromSeq))
    }

    public func unwatch(sessionID: String) async throws {
        _ = try await call(Proto.cmdSessionUnwatch, sessionID: sessionID)
    }

    // MARK: - Wire plumbing

    private func call(
        _ type: String,
        sessionID: String? = nil,
        payload: (any Encodable & Sendable)? = nil
    ) async throws -> WireResult {
        nextID += 1
        let id = String(nextID)

        var envelope = Envelope(id: id, sessionId: sessionID, type: type)
        if let payload {
            envelope.payload = try JSONValue.from(payload)
        }
        let data = try JSONEncoder.wingman.encode(envelope)

        return try await withCheckedThrowingContinuation { continuation in
            pendingReplies[id] = continuation
            Task {
                do {
                    try await self.sendEncrypted(data)
                } catch {
                    self.failPending(id: id, error: error)
                }
            }
        }
    }

    private func failPending(id: String, error: Error) {
        pendingReplies.removeValue(forKey: id)?.resume(throwing: error)
    }

    private func sendEncrypted(_ plaintext: Data) async throws {
        guard var noiseSession = session else { throw ClientError.notConnected }
        let ciphertext = try noiseSession.send.encrypt(ad: Data(), plaintext: plaintext)
        session = noiseSession
        try await send(raw: ciphertext)
    }

    private func send(raw data: Data) async throws {
        guard let task else { throw ClientError.notConnected }
        try await task.send(.data(data))
    }

    private func receiveRaw() async throws -> Data {
        guard let task else { throw ClientError.notConnected }
        switch try await task.receive() {
        case .data(let data): return data
        case .string(let string): return Data(string.utf8)
        @unknown default: throw ClientError.badReply
        }
    }

    private func readLoop() async {
        while task != nil {
            do {
                let ciphertext = try await receiveRaw()
                guard var noiseSession = session else { break }
                let plaintext = try noiseSession.receive.decrypt(ad: Data(), ciphertext: ciphertext)
                session = noiseSession
                dispatch(try JSONDecoder.wingman.decode(Envelope.self, from: plaintext))
            } catch {
                break
            }
        }
        eventContinuation.finish()
        disconnect()
    }

    private func dispatch(_ envelope: Envelope) {
        if envelope.type == Proto.typeRes, let id = envelope.id {
            guard let continuation = pendingReplies.removeValue(forKey: id) else { return }
            do {
                guard let payload = envelope.payload else { throw ClientError.badReply }
                let result = try payload.decode(WireResult.self)
                if result.ok {
                    continuation.resume(returning: result)
                } else {
                    continuation.resume(throwing: ClientError.commandFailed(result.error ?? "unknown error"))
                }
            } catch {
                continuation.resume(throwing: error)
            }
            return
        }
        eventContinuation.yield(envelope)
    }
}
