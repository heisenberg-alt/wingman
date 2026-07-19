import AVFoundation
import SwiftUI

/// First-run pairing: scan the QR from `wingmand pair`, or paste the payload.
struct PairingView: View {
    @EnvironmentObject private var store: AppStore
    @State private var deviceName = UIDevice.current.name
    @State private var pastedPayload = ""
    @State private var showScanner = false
    @State private var isPairing = false

    var body: some View {
        NavigationStack {
            VStack(spacing: 24) {
                Spacer()

                Image(systemName: "airplane.circle.fill")
                    .font(.system(size: 72))
                    .foregroundStyle(.tint)
                Text("Wingman")
                    .font(.largeTitle.bold())
                Text("Pair with your dev machine:\nrun `wingmand pair` and scan the QR code.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)

                Spacer()

                TextField("Device name", text: $deviceName)
                    .textFieldStyle(.roundedBorder)

                Button {
                    showScanner = true
                } label: {
                    Label("Scan QR code", systemImage: "qrcode.viewfinder")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)

                DisclosureGroup("Paste payload instead") {
                    TextField("Pairing payload JSON", text: $pastedPayload, axis: .vertical)
                        .textFieldStyle(.roundedBorder)
                        .font(.footnote.monospaced())
                        .lineLimit(3...6)
                    Button("Pair") {
                        pair(with: pastedPayload)
                    }
                    .buttonStyle(.bordered)
                    .disabled(pastedPayload.isEmpty || isPairing)
                }
                .font(.callout)
            }
            .padding(24)
            .sheet(isPresented: $showScanner) {
                QRScannerView { code in
                    showScanner = false
                    pair(with: code)
                }
            }
            .overlay {
                if isPairing {
                    ProgressView("Pairing…")
                        .padding(24)
                        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
                }
            }
        }
    }

    private func pair(with payload: String) {
        isPairing = true
        Task {
            await store.pair(payloadJSON: payload, deviceName: deviceName)
            isPairing = false
        }
    }
}

/// Camera-based QR scanner.
struct QRScannerView: UIViewControllerRepresentable {
    let onCode: (String) -> Void

    func makeUIViewController(context: Context) -> ScannerViewController {
        let controller = ScannerViewController()
        controller.onCode = onCode
        return controller
    }

    func updateUIViewController(_ controller: ScannerViewController, context: Context) {}
}

final class ScannerViewController: UIViewController, AVCaptureMetadataOutputObjectsDelegate {
    var onCode: ((String) -> Void)?
    private let captureSession = AVCaptureSession()
    private var delivered = false

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .black

        guard let device = AVCaptureDevice.default(for: .video),
              let input = try? AVCaptureDeviceInput(device: device),
              captureSession.canAddInput(input)
        else { return }
        captureSession.addInput(input)

        let output = AVCaptureMetadataOutput()
        guard captureSession.canAddOutput(output) else { return }
        captureSession.addOutput(output)
        output.setMetadataObjectsDelegate(self, queue: .main)
        output.metadataObjectTypes = [.qr]

        let preview = AVCaptureVideoPreviewLayer(session: captureSession)
        preview.frame = view.layer.bounds
        preview.videoGravity = .resizeAspectFill
        view.layer.addSublayer(preview)

        DispatchQueue.global(qos: .userInitiated).async { [captureSession] in
            captureSession.startRunning()
        }
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        captureSession.stopRunning()
    }

    func metadataOutput(
        _ output: AVCaptureMetadataOutput,
        didOutput metadataObjects: [AVMetadataObject],
        from connection: AVCaptureConnection
    ) {
        guard !delivered,
              let object = metadataObjects.first as? AVMetadataMachineReadableCodeObject,
              let value = object.stringValue
        else { return }
        delivered = true
        onCode?(value)
    }
}
