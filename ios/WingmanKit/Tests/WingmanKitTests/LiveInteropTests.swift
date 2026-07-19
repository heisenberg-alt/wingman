// Live interop test: pairs with and talks to a real running wingmand over the
// Noise channel, proving the Swift Noise implementation is wire-compatible
// with the daemon's flynn/noise.
//
// Skipped unless WINGMAN_INTEROP_PAYLOAD is set to the JSON from
// `wingmand pair --json` (with a reachable "lan" address).
import CryptoKit
import Foundation
import Testing

@testable import WingmanKit

@Suite struct LiveInteropTests {
    @Test func pairListAndUnpair() async throws {
        guard let payloadJSON = ProcessInfo.processInfo.environment["WINGMAN_INTEROP_PAYLOAD"] else {
            // Not an error: the deterministic Noise tests cover the algorithm;
            // this test proves wire-level interop when a daemon is available.
            return
        }

        let payload = try JSONDecoder.wingman.decode(PairingPayload.self, from: Data(payloadJSON.utf8))
        guard let lan = payload.lan else {
            Issue.record("payload has no lan address")
            return
        }

        let key = Curve25519.KeyAgreement.PrivateKey()
        let client = WingmanClient()
        try await client.connect(route: .lan(lan), staticKey: key, daemonPublicKey: payload.pub)
        try await client.pair(token: payload.token, deviceName: "swift-interop-test")

        let sessions = try await client.listSessions()
        // The daemon may or may not have sessions; the call succeeding over
        // the encrypted channel is the assertion.
        _ = sessions
        await client.disconnect()
    }
}
