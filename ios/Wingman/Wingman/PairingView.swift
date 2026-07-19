import AVFoundation
import SwiftUI

/// First-run pairing: scan the QR from `wingmand pair`, or paste the payload.
struct PairingView: View {
    @EnvironmentObject private var store: AppStore
    @State private var deviceName = UIDevice.current.name
    @State private var pastedPayload = ""
    @State private var showScanner = false
    @State private var isPairing = false
    @State private var appeared = false

    var body: some View {
        NavigationStack {
            ZStack {
                Brand.heroGradient
                    .ignoresSafeArea()

                VStack(spacing: 0) {
                    Spacer()

                    // Wing mark, echoing the app icon, with a staggered entrance.
                    WingMark(feather: 18)
                        .opacity(appeared ? 1 : 0)
                        .offset(x: appeared ? 0 : -40)
                        .animation(.spring(duration: 0.7, bounce: 0.25), value: appeared)

                    Text("Wingman")
                        .font(Brand.display(40))
                        .foregroundStyle(.white)
                        .padding(.top, 26)
                        .opacity(appeared ? 1 : 0)
                        .animation(.easeOut(duration: 0.6).delay(0.15), value: appeared)

                    Text("Your seat beside Copilot,\nwherever you are.")
                        .font(.callout)
                        .foregroundStyle(.white.opacity(0.75))
                        .multilineTextAlignment(.center)
                        .padding(.top, 6)
                        .opacity(appeared ? 1 : 0)
                        .animation(.easeOut(duration: 0.6).delay(0.3), value: appeared)

                    Spacer()

                    VStack(spacing: 14) {
                        Text("Run `wingmand pair` on your dev machine, then scan the QR code.")
                            .font(.footnote)
                            .foregroundStyle(.white.opacity(0.7))
                            .multilineTextAlignment(.center)

                        TextField("Device name", text: $deviceName)
                            .textFieldStyle(.plain)
                            .padding(12)
                            .background(.white.opacity(0.12), in: RoundedRectangle(cornerRadius: 12))
                            .foregroundStyle(.white)

                        Button {
                            showScanner = true
                        } label: {
                            Label("Scan QR code", systemImage: "qrcode.viewfinder")
                                .font(.headline)
                                .frame(maxWidth: .infinity)
                                .padding(.vertical, 6)
                        }
                        .buttonStyle(.borderedProminent)
                        .buttonBorderShape(.capsule)
                        .tint(.white)
                        .foregroundStyle(Color(red: 0.2, green: 0.19, blue: 0.6))

                        DisclosureGroup {
                            TextField("Pairing payload JSON", text: $pastedPayload, axis: .vertical)
                                .textFieldStyle(.plain)
                                .font(.footnote.monospaced())
                                .lineLimit(3...6)
                                .padding(10)
                                .background(.white.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
                                .foregroundStyle(.white)
                            Button("Pair with payload") {
                                pair(with: pastedPayload)
                            }
                            .font(.callout.bold())
                            .foregroundStyle(.white)
                            .padding(.top, 8)
                            .disabled(pastedPayload.isEmpty || isPairing)
                        } label: {
                            Text("Paste payload instead")
                                .font(.footnote)
                                .foregroundStyle(.white.opacity(0.7))
                        }
                        .tint(.white.opacity(0.7))
                    }
                    .padding(24)
                }
            }
            .sheet(isPresented: $showScanner) {
                QRScannerView { code in
                    showScanner = false
                    pair(with: code)
                }
                .ignoresSafeArea()
            }
            .onAppear { appeared = true }
            .overlay {
                if isPairing {
                    ProgressView("Pairing…")
                        .padding(24)
                        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 14))
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
