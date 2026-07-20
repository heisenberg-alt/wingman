import SwiftUI
import UIKit

/// Renders agent Markdown: inline styling via AttributedString, fenced code
/// blocks as styled, copyable panels.
struct RichText: View {
    @Environment(\.surfaces) private var surfaces
    let text: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Self.segments(of: text)) { segment in
                switch segment.kind {
                case .prose:
                    Text(Self.inline(segment.content))
                        .textSelection(.enabled)
                case .code:
                    CodeBlock(code: segment.content, language: segment.language)
                }
            }
        }
    }

    /// Parses inline Markdown, falling back to plain text on failure.
    static func inline(_ string: String) -> AttributedString {
        let options = AttributedString.MarkdownParsingOptions(
            interpretedSyntax: .inlineOnlyPreservingWhitespace
        )
        return (try? AttributedString(markdown: string, options: options))
            ?? AttributedString(string)
    }

    // MARK: - Fenced code block segmentation

    struct Segment: Identifiable {
        enum Kind { case prose, code }
        let id: Int
        let kind: Kind
        let content: String
        var language: String = ""
    }

    /// Splits text into prose and fenced (```lang ... ```) code segments.
    static func segments(of text: String) -> [Segment] {
        var segments: [Segment] = []
        var prose: [String] = []
        var code: [String] = []
        var language = ""
        var inCode = false

        func flushProse() {
            let joined = prose.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            if !joined.isEmpty {
                segments.append(Segment(id: segments.count, kind: .prose, content: joined))
            }
            prose = []
        }

        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("```") {
                if inCode {
                    segments.append(Segment(
                        id: segments.count,
                        kind: .code,
                        content: code.joined(separator: "\n"),
                        language: language
                    ))
                    code = []
                    inCode = false
                } else {
                    flushProse()
                    language = String(trimmed.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                    inCode = true
                }
                continue
            }
            if inCode {
                code.append(String(line))
            } else {
                prose.append(String(line))
            }
        }
        // Unterminated fence (mid-stream): render what we have as code.
        if inCode {
            segments.append(Segment(id: segments.count, kind: .code, content: code.joined(separator: "\n"), language: language))
        } else {
            flushProse()
        }
        return segments
    }
}

/// A styled, copyable code panel.
struct CodeBlock: View {
    @Environment(\.surfaces) private var surfaces
    let code: String
    let language: String
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text(language.isEmpty ? "code" : language)
                    .font(.caption2.smallCaps())
                    .foregroundStyle(.secondary)
                Spacer()
                Button {
                    UIPasteboard.general.string = code
                    copied = true
                    Task { @MainActor in
                        try? await Task.sleep(for: .seconds(1.5))
                        copied = false
                    }
                } label: {
                    Label(copied ? "Copied" : "Copy", systemImage: copied ? "checkmark" : "doc.on.doc")
                        .font(.caption2)
                }
                .buttonStyle(.plain)
                .foregroundStyle(copied ? .green : .secondary)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 6)

            Divider()

            ScrollView(.horizontal, showsIndicators: false) {
                Text(code)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                    .padding(12)
            }
        }
        .background(surfaces.card.opacity(0.6), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.quaternary))
    }
}
