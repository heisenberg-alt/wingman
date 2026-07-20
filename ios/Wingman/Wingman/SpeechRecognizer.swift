import AVFoundation
import Speech
import SwiftUI

/// Live speech-to-text for dictating prompts, built on SFSpeechRecognizer
/// with on-device recognition where available.
@MainActor
final class SpeechRecognizer: ObservableObject {
    /// Text recognized since recording started.
    @Published var transcript = ""
    @Published var isRecording = false
    @Published var errorMessage: String?

    private let recognizer = SFSpeechRecognizer()
    private var audioEngine: AVAudioEngine?
    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?

    var isAvailable: Bool {
        recognizer?.isAvailable ?? false
    }

    func toggle() {
        if isRecording {
            stop()
        } else {
            Task { await start() }
        }
    }

    func start() async {
        errorMessage = nil
        transcript = ""

        guard await requestAuthorization() else {
            errorMessage = "Enable microphone and speech recognition for Wingman in Settings."
            return
        }
        guard let recognizer, recognizer.isAvailable else {
            errorMessage = "Speech recognition is unavailable on this device."
            return
        }

        do {
            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.record, mode: .measurement, options: .duckOthers)
            try session.setActive(true, options: .notifyOthersOnDeactivation)

            let engine = AVAudioEngine()
            let request = SFSpeechAudioBufferRecognitionRequest()
            request.shouldReportPartialResults = true
            if recognizer.supportsOnDeviceRecognition {
                request.requiresOnDeviceRecognition = true
            }

            self.audioEngine = engine
            self.request = request

            let inputNode = engine.inputNode
            let format = inputNode.outputFormat(forBus: 0)
            inputNode.installTap(onBus: 0, bufferSize: 1024, format: format) { buffer, _ in
                request.append(buffer)
            }

            engine.prepare()
            try engine.start()

            self.isRecording = true

            self.task = recognizer.recognitionTask(with: request) { [weak self] result, error in
                Task { @MainActor [weak self] in
                    guard let self else { return }
                    if let result {
                        self.transcript = result.bestTranscription.formattedString
                    }
                    if error != nil || (result?.isFinal ?? false) {
                        self.stop()
                    }
                }
            }
        } catch {
            errorMessage = "Could not start recording: \(error.localizedDescription)"
            stop()
        }
    }

    func stop() {
        audioEngine?.stop()
        audioEngine?.inputNode.removeTap(onBus: 0)
        request?.endAudio()
        task?.cancel()
        audioEngine = nil
        request = nil
        task = nil
        isRecording = false
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }

    private func requestAuthorization() async -> Bool {
        let speech = await withCheckedContinuation { continuation in
            SFSpeechRecognizer.requestAuthorization { status in
                continuation.resume(returning: status == .authorized)
            }
        }
        guard speech else { return false }
        return await AVAudioApplication.requestRecordPermission()
    }
}

/// Microphone button for the composer: idle mic, pulsing red stop while
/// recording.
struct MicButton: View {
    @ObservedObject var speech: SpeechRecognizer
    @State private var pulsing = false

    var body: some View {
        Button {
            speech.toggle()
        } label: {
            Image(systemName: speech.isRecording ? "stop.fill" : "mic.fill")
                .font(.body.bold())
                .foregroundStyle(.white)
                .frame(width: 34, height: 34)
                .background(speech.isRecording ? Color.red : Color.secondary.opacity(0.6), in: Circle())
                .scaleEffect(speech.isRecording && pulsing ? 1.1 : 1.0)
        }
        .onChange(of: speech.isRecording) { _, recording in
            if recording {
                withAnimation(.easeInOut(duration: 0.6).repeatForever(autoreverses: true)) {
                    pulsing = true
                }
            } else {
                withAnimation(.default) { pulsing = false }
            }
        }
        .accessibilityLabel(speech.isRecording ? "Stop dictation" : "Dictate prompt")
    }
}
