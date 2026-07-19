// Noise XX protocol implementation (X25519, ChaCha20-Poly1305, SHA-256) built
// on CryptoKit, wire-compatible with the daemon's flynn/noise implementation
// of Noise_XX_25519_ChaChaPoly_SHA256.
import CryptoKit
import Foundation

public enum NoiseError: Error, Equatable {
    case malformedMessage
    case decryptFailed
    case peerKeyMismatch
}

/// One direction of post-handshake transport encryption.
public struct NoiseCipherState: Sendable {
    var key: SymmetricKey?
    var nonce: UInt64 = 0

    init(key: SymmetricKey? = nil) {
        self.key = key
    }

    /// Noise nonce: 4 zero bytes followed by the 64-bit little-endian counter.
    static func nonceData(_ counter: UInt64) -> Data {
        var data = Data(count: 4)
        var le = counter.littleEndian
        withUnsafeBytes(of: &le) { data.append(contentsOf: $0) }
        return data
    }

    public mutating func encrypt(ad: Data, plaintext: Data) throws -> Data {
        guard let key else { return plaintext } // handshake phase before any key
        let box = try ChaChaPoly.seal(
            plaintext,
            using: key,
            nonce: ChaChaPoly.Nonce(data: Self.nonceData(nonce)),
            authenticating: ad
        )
        nonce += 1
        return box.ciphertext + box.tag
    }

    public mutating func decrypt(ad: Data, ciphertext: Data) throws -> Data {
        guard let key else { return ciphertext }
        guard ciphertext.count >= 16 else { throw NoiseError.malformedMessage }
        let box = try ChaChaPoly.SealedBox(
            nonce: ChaChaPoly.Nonce(data: Self.nonceData(nonce)),
            ciphertext: ciphertext.dropLast(16),
            tag: ciphertext.suffix(16)
        )
        guard let plaintext = try? ChaChaPoly.open(box, using: key, authenticating: ad) else {
            throw NoiseError.decryptFailed
        }
        nonce += 1
        return plaintext
    }
}

/// Completed handshake: transport ciphers plus the authenticated peer key.
public struct NoiseSession: Sendable {
    public var send: NoiseCipherState
    public var receive: NoiseCipherState
    public let remoteStaticKey: Data
}

// MARK: - Symmetric state

private func hmacSHA256(key: Data, data: Data) -> Data {
    Data(HMAC<SHA256>.authenticationCode(for: data, using: SymmetricKey(data: key)))
}

/// Noise HKDF with two outputs.
private func noiseHKDF(chainingKey: Data, inputKeyMaterial: Data) -> (Data, Data) {
    let temp = hmacSHA256(key: chainingKey, data: inputKeyMaterial)
    let out1 = hmacSHA256(key: temp, data: Data([0x01]))
    let out2 = hmacSHA256(key: temp, data: out1 + Data([0x02]))
    return (out1, out2)
}

struct SymmetricState {
    var cipher = NoiseCipherState()
    var chainingKey: Data
    var handshakeHash: Data

    init(protocolName: String) {
        let name = Data(protocolName.utf8)
        if name.count <= 32 {
            handshakeHash = name + Data(count: 32 - name.count)
        } else {
            handshakeHash = Data(SHA256.hash(data: name))
        }
        chainingKey = handshakeHash
        mixHash(Data()) // empty prologue
    }

    mutating func mixHash(_ data: Data) {
        handshakeHash = Data(SHA256.hash(data: handshakeHash + data))
    }

    mutating func mixKey(_ inputKeyMaterial: Data) {
        let (ck, temp) = noiseHKDF(chainingKey: chainingKey, inputKeyMaterial: inputKeyMaterial)
        chainingKey = ck
        cipher = NoiseCipherState(key: SymmetricKey(data: temp))
    }

    mutating func encryptAndHash(_ plaintext: Data) throws -> Data {
        let ciphertext = try cipher.encrypt(ad: handshakeHash, plaintext: plaintext)
        mixHash(ciphertext)
        return ciphertext
    }

    mutating func decryptAndHash(_ ciphertext: Data) throws -> Data {
        let plaintext = try cipher.decrypt(ad: handshakeHash, ciphertext: ciphertext)
        mixHash(ciphertext)
        return plaintext
    }

    mutating func split() -> (NoiseCipherState, NoiseCipherState) {
        let (k1, k2) = noiseHKDF(chainingKey: chainingKey, inputKeyMaterial: Data())
        return (
            NoiseCipherState(key: SymmetricKey(data: k1)),
            NoiseCipherState(key: SymmetricKey(data: k2))
        )
    }
}

private let protocolName = "Noise_XX_25519_ChaChaPoly_SHA256"

private func dh(
    _ privateKey: Curve25519.KeyAgreement.PrivateKey,
    _ publicKeyData: Data
) throws -> Data {
    let publicKey = try Curve25519.KeyAgreement.PublicKey(rawRepresentation: publicKeyData)
    let shared = try privateKey.sharedSecretFromKeyAgreement(with: publicKey)
    return shared.withUnsafeBytes { Data($0) }
}

// MARK: - Initiator (the phone)

/// Initiator side of the Noise XX handshake.
///
///     -> e
///     <- e, ee, s, es
///     -> s, se
public struct NoiseHandshakeInitiator {
    private var ss = SymmetricState(protocolName: protocolName)
    private let staticKey: Curve25519.KeyAgreement.PrivateKey
    private let ephemeralKey: Curve25519.KeyAgreement.PrivateKey
    private var remoteEphemeral = Data()
    private var remoteStatic = Data()

    public init(
        staticKey: Curve25519.KeyAgreement.PrivateKey,
        ephemeralKey: Curve25519.KeyAgreement.PrivateKey = .init()
    ) {
        self.staticKey = staticKey
        self.ephemeralKey = ephemeralKey
    }

    public mutating func writeMessage1() throws -> Data {
        let e = ephemeralKey.publicKey.rawRepresentation
        ss.mixHash(e)
        let payload = try ss.encryptAndHash(Data())
        return e + payload
    }

    public mutating func readMessage2(_ message: Data) throws {
        // 32 (re) + 48 (encrypted rs) + 16 (encrypted empty payload)
        guard message.count >= 96 else { throw NoiseError.malformedMessage }
        let message = Data(message) // normalize indices

        let re = message.prefix(32)
        remoteEphemeral = Data(re)
        ss.mixHash(remoteEphemeral)
        try ss.mixKey(dh(ephemeralKey, remoteEphemeral)) // ee

        let encryptedStatic = message.dropFirst(32).prefix(48)
        remoteStatic = try ss.decryptAndHash(Data(encryptedStatic))
        try ss.mixKey(dh(ephemeralKey, remoteStatic)) // es

        _ = try ss.decryptAndHash(Data(message.dropFirst(80)))
    }

    public mutating func writeMessage3() throws -> (message: Data, session: NoiseSession) {
        let encryptedStatic = try ss.encryptAndHash(staticKey.publicKey.rawRepresentation) // s
        try ss.mixKey(dh(staticKey, remoteEphemeral)) // se
        let payload = try ss.encryptAndHash(Data())

        let (c1, c2) = ss.split()
        // Initiator sends with the first cipher, receives with the second.
        let session = NoiseSession(send: c1, receive: c2, remoteStaticKey: remoteStatic)
        return (encryptedStatic + payload, session)
    }
}

// MARK: - Responder (used by tests; the daemon is the responder in production)

public struct NoiseHandshakeResponder {
    private var ss = SymmetricState(protocolName: protocolName)
    private let staticKey: Curve25519.KeyAgreement.PrivateKey
    private let ephemeralKey: Curve25519.KeyAgreement.PrivateKey
    private var remoteEphemeral = Data()

    public init(
        staticKey: Curve25519.KeyAgreement.PrivateKey,
        ephemeralKey: Curve25519.KeyAgreement.PrivateKey = .init()
    ) {
        self.staticKey = staticKey
        self.ephemeralKey = ephemeralKey
    }

    public mutating func readMessage1(_ message: Data) throws {
        guard message.count >= 32 else { throw NoiseError.malformedMessage }
        let message = Data(message)
        remoteEphemeral = Data(message.prefix(32))
        ss.mixHash(remoteEphemeral)
        _ = try ss.decryptAndHash(Data(message.dropFirst(32)))
    }

    public mutating func writeMessage2() throws -> Data {
        let e = ephemeralKey.publicKey.rawRepresentation
        ss.mixHash(e)
        try ss.mixKey(dh(ephemeralKey, remoteEphemeral)) // ee
        let encryptedStatic = try ss.encryptAndHash(staticKey.publicKey.rawRepresentation) // s
        try ss.mixKey(dh(staticKey, remoteEphemeral)) // es
        let payload = try ss.encryptAndHash(Data())
        return e + encryptedStatic + payload
    }

    public mutating func readMessage3(_ message: Data) throws -> NoiseSession {
        guard message.count >= 64 else { throw NoiseError.malformedMessage }
        let message = Data(message)
        let remoteStatic = try ss.decryptAndHash(Data(message.prefix(48)))
        try ss.mixKey(dh(ephemeralKey, remoteStatic)) // se
        _ = try ss.decryptAndHash(Data(message.dropFirst(48)))

        let (c1, c2) = ss.split()
        // Responder receives with the first cipher, sends with the second.
        return NoiseSession(send: c2, receive: c1, remoteStaticKey: remoteStatic)
    }
}
