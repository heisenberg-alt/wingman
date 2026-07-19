import SwiftUI

/// App-wide theme selection, persisted in AppStorage.
enum AppTheme: String, CaseIterable, Identifiable {
    case system
    case light
    case dark
    case midnight

    var id: String { rawValue }

    var label: String {
        switch self {
        case .system: return "System"
        case .light: return "Light"
        case .dark: return "Dark"
        case .midnight: return "Midnight"
        }
    }

    var symbol: String {
        switch self {
        case .system: return "circle.lefthalf.filled"
        case .light: return "sun.max.fill"
        case .dark: return "moon.fill"
        case .midnight: return "sparkles"
        }
    }

    /// The color scheme forced by this theme; nil follows the system.
    var colorScheme: ColorScheme? {
        switch self {
        case .system: return nil
        case .light: return .light
        case .dark, .midnight: return .dark
        }
    }

    /// Midnight uses true-black canvases with indigo accents (OLED friendly).
    var isMidnight: Bool { self == .midnight }
}

/// Brand palette and shared style constants.
enum Brand {
    static let indigoDeep = Color(red: 0.09, green: 0.10, blue: 0.24)
    static let indigo = Color(red: 0.36, green: 0.33, blue: 0.91)
    static let heroGradient = LinearGradient(
        colors: [indigoDeep, indigo],
        startPoint: .topLeading,
        endPoint: .bottomTrailing
    )

    static let cardCornerRadius: CGFloat = 16
}

/// Semantic surface colors that adapt to the selected theme.
struct Surfaces {
    let theme: AppTheme

    /// Scrollable canvas behind cards and transcripts.
    var canvas: Color {
        theme.isMidnight ? .black : Color(.systemGroupedBackground)
    }

    /// Elevated card / bubble background.
    var card: Color {
        theme.isMidnight ? Color(white: 0.09) : Color(.secondarySystemGroupedBackground)
    }
}

private struct ThemeKey: EnvironmentKey {
    static let defaultValue = AppTheme.system
}

extension EnvironmentValues {
    var appTheme: AppTheme {
        get { self[ThemeKey.self] }
        set { self[ThemeKey.self] = newValue }
    }

    var surfaces: Surfaces { Surfaces(theme: appTheme) }
}
