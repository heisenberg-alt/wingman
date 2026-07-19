// Wire protocol v1 types, mirroring daemon/internal/proto (docs/PROTOCOL.md).
import Foundation

public enum Proto {
    public static let version = 1

    // Commands (phone → daemon).
    public static let cmdSessionList = "session.list"
    public static let cmdSessionCreate = "session.create"
    public static let cmdSessionPrompt = "session.prompt"
    public static let cmdSessionApprove = "session.approve"
    public static let cmdSessionCancel = "session.cancel"
    public static let cmdSessionWatch = "session.watch"
    public static let cmdSessionUnwatch = "session.unwatch"
    public static let cmdPairRequest = "pair.request"

    // Events (daemon → phone).
    public static let typeRes = "res"
    public static let evtSessionState = "session.state"
    public static let evtTranscriptDelta = "transcript.delta"
    public static let evtPermissionRequest = "permission.request"
    public static let evtPermissionResolved = "permission.resolved"
    public static let evtTurnEnded = "turn.ended"
}

public struct Envelope: Codable, Sendable {
    public var v: Int
    public var id: String?
    public var sessionId: String?
    public var seq: UInt64?
    public var type: String
    public var payload: JSONValue?

    public init(id: String? = nil, sessionId: String? = nil, type: String, payload: JSONValue? = nil) {
        self.v = Proto.version
        self.id = id
        self.sessionId = sessionId
        self.seq = nil
        self.type = type
        self.payload = payload
    }
}

public struct WireResult: Codable, Sendable {
    public var ok: Bool
    public var error: String?
    public var data: JSONValue?
}

public struct SessionInfo: Codable, Sendable, Identifiable, Hashable {
    public var id: String
    public var cwd: String
    public var status: String
    public var createdAt: Date
}

public struct SessionList: Codable, Sendable {
    public var sessions: [SessionInfo]
}

public struct SessionCreate: Codable, Sendable {
    public var cwd: String
    public var prompt: String?
    public init(cwd: String, prompt: String? = nil) {
        self.cwd = cwd
        self.prompt = prompt
    }
}

public struct SessionPrompt: Codable, Sendable {
    public var text: String
    public init(text: String) { self.text = text }
}

public struct SessionApprove: Codable, Sendable {
    public var requestId: String
    public var optionId: String
    public init(requestId: String, optionId: String) {
        self.requestId = requestId
        self.optionId = optionId
    }
}

public struct SessionWatch: Codable, Sendable {
    public var fromSeq: UInt64
    public init(fromSeq: UInt64) { self.fromSeq = fromSeq }
}

public struct PairRequest: Codable, Sendable {
    public var token: String
    public var deviceName: String
    public init(token: String, deviceName: String) {
        self.token = token
        self.deviceName = deviceName
    }
}

public struct SessionState: Codable, Sendable {
    public var status: String
}

public struct TranscriptDelta: Codable, Sendable {
    public var kind: String
    public var data: JSONValue
}

public struct PermissionOption: Codable, Sendable, Identifiable, Hashable {
    public var optionId: String
    public var name: String
    public var kind: String
    public var id: String { optionId }
}

public struct PermissionRequest: Codable, Sendable, Identifiable {
    public var requestId: String
    public var title: String?
    public var options: [PermissionOption]
    public var id: String { requestId }
}

public struct PermissionResolved: Codable, Sendable {
    public var requestId: String
    public var optionId: String?
    public var resolvedBy: String
}

public struct TurnEnded: Codable, Sendable {
    public var stopReason: String
}

/// Pairing payload scanned from the daemon's QR code.
public struct PairingPayload: Codable, Sendable {
    public var v: Int
    public var pub: Data // base64 in JSON, matching Go []byte encoding
    public var lan: String?
    public var relay: String?
    public var room: String
    public var token: String
}

// MARK: - JSONValue

/// A JSON fragment that round-trips unknown structures (Go json.RawMessage).
public indirect enum JSONValue: Codable, Sendable, Equatable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let value = try? container.decode(Bool.self) {
            self = .bool(value)
        } else if let value = try? container.decode(Double.self) {
            self = .number(value)
        } else if let value = try? container.decode(String.self) {
            self = .string(value)
        } else if let value = try? container.decode([JSONValue].self) {
            self = .array(value)
        } else {
            self = .object(try container.decode([String: JSONValue].self))
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .null: try container.encodeNil()
        case .bool(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .object(let value): try container.encode(value)
        }
    }

    public subscript(key: String) -> JSONValue? {
        if case .object(let dict) = self { return dict[key] }
        return nil
    }

    public var stringValue: String? {
        if case .string(let value) = self { return value }
        return nil
    }

    /// Encodes an Encodable payload into a JSONValue.
    public static func from<T: Encodable>(_ value: T) throws -> JSONValue {
        let data = try JSONEncoder.wingman.encode(value)
        return try JSONDecoder.wingman.decode(JSONValue.self, from: data)
    }

    /// Decodes this fragment into a concrete type.
    public func decode<T: Decodable>(_ type: T.Type) throws -> T {
        let data = try JSONEncoder.wingman.encode(self)
        return try JSONDecoder.wingman.decode(type, from: data)
    }
}

extension JSONEncoder {
    /// Encoder matching the daemon's JSON conventions (RFC 3339 dates).
    public static let wingman: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }()
}

extension JSONDecoder {
    /// Decoder tolerant of Go's RFC 3339 timestamps with fractional seconds.
    public static let wingman: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let value = try decoder.singleValueContainer().decode(String.self)
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = formatter.date(from: value) {
                return date
            }
            formatter.formatOptions = [.withInternetDateTime]
            if let date = formatter.date(from: value) {
                return date
            }
            throw DecodingError.dataCorrupted(.init(
                codingPath: decoder.codingPath,
                debugDescription: "unparseable date: \(value)"
            ))
        }
        return decoder
    }()
}
