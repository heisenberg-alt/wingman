import UIKit
import UserNotifications

/// Bridges APNs and UNUserNotificationCenter to the app: collects the device
/// token, registers the approval notification category, and turns lock-screen
/// Approve/Deny actions into callbacks (ADR-0006).
final class PushCoordinator: NSObject, UNUserNotificationCenterDelegate {
    static let shared = PushCoordinator()

    static let approvalCategory = "WINGMAN_APPROVAL"
    static let approveAction = "WINGMAN_APPROVE"
    static let denyAction = "WINGMAN_DENY"

    /// The latest APNs token as hex, once iOS delivers it.
    private(set) var deviceToken: String?
    /// Called on token arrival (and re-issue) so the store can register it.
    var onToken: ((String) -> Void)?
    /// Called for approval decisions taken from a notification:
    /// (sessionId, requestId, optionId).
    var onApproval: ((String, String, String) -> Void)?
    /// Called when the user taps the notification body: (sessionId).
    var onOpenSession: ((String) -> Void)?

    /// Requests notification authorization, registers the approval category,
    /// and asks iOS for a remote-notification token.
    func activate() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self

        let approve = UNNotificationAction(
            identifier: Self.approveAction,
            title: "Allow once",
            options: [.authenticationRequired]
        )
        let deny = UNNotificationAction(
            identifier: Self.denyAction,
            title: "Deny",
            options: [.destructive, .authenticationRequired]
        )
        center.setNotificationCategories([
            UNNotificationCategory(
                identifier: Self.approvalCategory,
                actions: [approve, deny],
                intentIdentifiers: []
            ),
        ])

        Task {
            let granted = (try? await center.requestAuthorization(options: [.alert, .sound, .badge])) ?? false
            guard granted else { return }
            await MainActor.run {
                UIApplication.shared.registerForRemoteNotifications()
            }
        }
    }

    func tokenReceived(_ token: Data) {
        let hex = token.map { String(format: "%02x", $0) }.joined()
        deviceToken = hex
        onToken?(hex)
    }

    // Show approval alerts even while the app is foregrounded; the in-app
    // sheet appears too, and iOS coalesces gracefully.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification
    ) async -> UNNotificationPresentationOptions {
        [.banner, .sound]
    }

    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse
    ) async {
        let info = response.notification.request.content.userInfo
        guard let sessionID = info["sessionId"] as? String else { return }
        guard let requestID = info["requestId"] as? String else {
            onOpenSession?(sessionID)
            return
        }
        let options = info["options"] as? [String] ?? []

        switch response.actionIdentifier {
        case Self.approveAction:
            onApproval?(sessionID, requestID, pick(options, prefix: "allow", fallback: "allow_once"))
        case Self.denyAction:
            onApproval?(sessionID, requestID, pick(options, prefix: "reject", fallback: "reject_once"))
        default:
            // Tapping the body opens the session's approval sheet.
            onOpenSession?(sessionID)
        }
    }

    /// Picks the first offered option id with the given prefix, since the
    /// exact ids (allow_once, allow_always, …) come from the Copilot CLI.
    private func pick(_ options: [String], prefix: String, fallback: String) -> String {
        options.first { $0.hasPrefix(prefix) } ?? fallback
    }
}
