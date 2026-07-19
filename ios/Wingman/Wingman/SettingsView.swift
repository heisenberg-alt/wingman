import SwiftUI

/// Settings: theme picker, connection details, and device management.
struct SettingsView: View {
    @EnvironmentObject private var store: AppStore
    @Environment(\.dismiss) private var dismiss
    @AppStorage("appTheme") private var themeRaw = AppTheme.system.rawValue

    private var theme: AppTheme { AppTheme(rawValue: themeRaw) ?? .system }

    var body: some View {
        NavigationStack {
            Form {
                Section("Appearance") {
                    ForEach(AppTheme.allCases) { option in
                        Button {
                            themeRaw = option.rawValue
                        } label: {
                            HStack {
                                Image(systemName: option.symbol)
                                    .frame(width: 26)
                                    .foregroundStyle(.tint)
                                Text(option.label)
                                    .foregroundStyle(.primary)
                                Spacer()
                                if option == theme {
                                    Image(systemName: "checkmark")
                                        .foregroundStyle(.tint)
                                }
                            }
                        }
                    }
                }

                Section("Connection") {
                    LabeledContent("Status") {
                        ConnectionBadge(state: store.connection)
                    }
                    if let config = store.config {
                        if let lan = config.lan, !lan.isEmpty {
                            LabeledContent("LAN", value: lan)
                        }
                        if let relay = config.relay, !relay.isEmpty {
                            LabeledContent("Relay", value: relay)
                        }
                        LabeledContent("Device name", value: config.deviceName)
                    }
                }

                Section {
                    Button("Unpair this device", role: .destructive) {
                        store.unpair()
                        dismiss()
                    }
                } footer: {
                    Text("Unpairing removes this device's keys. You will need to scan a new QR code to reconnect.")
                }

                Section("About") {
                    LabeledContent("App", value: "Wingman")
                    LabeledContent("Version", value: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "dev")
                }
            }
            .navigationTitle("Settings")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
    }
}
