import SwiftUI

@main
struct WingmanApp: App {
    @StateObject private var store = AppStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(store)
        }
    }
}

struct RootView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        Group {
            if store.config == nil {
                PairingView()
            } else {
                SessionListView()
            }
        }
        .task {
            #if DEBUG
            // Dev/UI-test hook: auto-pair from an environment payload, since
            // the simulator has no camera for the QR flow.
            if store.config == nil,
               let payload = ProcessInfo.processInfo.environment["WINGMAN_PAIR_PAYLOAD"] {
                await store.pair(payloadJSON: payload, deviceName: "simulator")
            }
            #endif
            await store.connect()
        }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active {
                Task { await store.connect() }
            }
        }
        .alert(
            "Error",
            isPresented: Binding(
                get: { store.lastError != nil },
                set: { if !$0 { store.lastError = nil } }
            )
        ) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(store.lastError ?? "")
        }
    }
}
