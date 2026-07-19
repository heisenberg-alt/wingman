import CryptoKit
import Foundation
import Testing

@testable import WingmanKit

@Suite struct NoiseTests {
    @Test func handshakeAndTransportRoundTrip() throws {
        let clientStatic = Curve25519.KeyAgreement.PrivateKey()
        let serverStatic = Curve25519.KeyAgreement.PrivateKey()

        var initiator = NoiseHandshakeInitiator(staticKey: clientStatic)
        var responder = NoiseHandshakeResponder(staticKey: serverStatic)

        let message1 = try initiator.writeMessage1()
        try responder.readMessage1(message1)
        let message2 = try responder.writeMessage2()
        try initiator.readMessage2(message2)
        let (message3, clientSession) = try initiator.writeMessage3()
        var serverSession = try responder.readMessage3(message3)
        var client = clientSession

        // Mutual authentication.
        #expect(client.remoteStaticKey == serverStatic.publicKey.rawRepresentation)
        #expect(serverSession.remoteStaticKey == clientStatic.publicKey.rawRepresentation)

        // Bidirectional transport with advancing nonces.
        for i in 0..<5 {
            let ping = Data("ping-\(i)".utf8)
            let ct = try client.send.encrypt(ad: Data(), plaintext: ping)
            #expect(try serverSession.receive.decrypt(ad: Data(), ciphertext: ct) == ping)

            let pong = Data("pong-\(i)".utf8)
            let ct2 = try serverSession.send.encrypt(ad: Data(), plaintext: pong)
            #expect(try client.receive.decrypt(ad: Data(), ciphertext: ct2) == pong)
        }
    }

    @Test func tamperedCiphertextRejected() throws {
        let clientStatic = Curve25519.KeyAgreement.PrivateKey()
        let serverStatic = Curve25519.KeyAgreement.PrivateKey()

        var initiator = NoiseHandshakeInitiator(staticKey: clientStatic)
        var responder = NoiseHandshakeResponder(staticKey: serverStatic)
        try responder.readMessage1(initiator.writeMessage1())
        try initiator.readMessage2(responder.writeMessage2())
        let (message3, clientSession) = try initiator.writeMessage3()
        var serverSession = try responder.readMessage3(message3)
        var client = clientSession

        var ct = try client.send.encrypt(ad: Data(), plaintext: Data("secret".utf8))
        ct[0] ^= 0xFF
        #expect(throws: NoiseError.decryptFailed) {
            _ = try serverSession.receive.decrypt(ad: Data(), ciphertext: ct)
        }
    }

    @Test func truncatedHandshakeMessagesRejected() throws {
        let key = Curve25519.KeyAgreement.PrivateKey()
        var initiator = NoiseHandshakeInitiator(staticKey: key)
        _ = try initiator.writeMessage1()
        #expect(throws: NoiseError.malformedMessage) {
            try initiator.readMessage2(Data(count: 40))
        }
    }
}

@Suite struct ProtocolTests {
    @Test func envelopeRoundTrip() throws {
        var envelope = Envelope(id: "7", sessionId: "abc", type: Proto.cmdSessionPrompt)
        envelope.payload = try JSONValue.from(SessionPrompt(text: "hello"))

        let data = try JSONEncoder.wingman.encode(envelope)
        let decoded = try JSONDecoder.wingman.decode(Envelope.self, from: data)

        #expect(decoded.v == 1)
        #expect(decoded.id == "7")
        #expect(decoded.sessionId == "abc")
        #expect(try decoded.payload?.decode(SessionPrompt.self).text == "hello")
    }

    @Test func decodesGoSessionInfo() throws {
        // Exactly as the Go daemon marshals proto.SessionInfo.
        let json = #"{"id":"68b27abd","cwd":"/tmp/x","status":"idle","createdAt":"2026-07-19T13:08:03.123456Z"}"#
        let info = try JSONDecoder.wingman.decode(SessionInfo.self, from: Data(json.utf8))
        #expect(info.id == "68b27abd")
        #expect(info.status == "idle")
    }

    @Test func decodesGoPairingPayload() throws {
        // Shape produced by `wingmand pair --json`; values are synthetic.
        let syntheticKey = Data(repeating: 0x41, count: 32).base64EncodedString()
        let json = #"{"v":1,"pub":"\#(syntheticKey)","lan":"192.0.2.10:7421","relay":"ws://relay.example:8443","room":"AAAAAAAAAAAAAAAA","token":"0000000000000000"}"#
        let payload = try JSONDecoder.wingman.decode(PairingPayload.self, from: Data(json.utf8))
        #expect(payload.pub.count == 32)
        #expect(payload.room == "AAAAAAAAAAAAAAAA")
        #expect(payload.lan == "192.0.2.10:7421")
    }

    @Test func permissionRequestDecoding() throws {
        let json = #"{"requestId":"r1","title":"Create file","options":[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"},{"optionId":"reject_once","name":"Deny","kind":"reject_once"}]}"#
        let request = try JSONDecoder.wingman.decode(PermissionRequest.self, from: Data(json.utf8))
        #expect(request.options.count == 2)
        #expect(request.options[0].optionId == "allow_once")
    }
}
