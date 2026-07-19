// Renders a QR code PNG from a string. Usage: swift qrpng.swift <text> <out.png>
import AppKit
import CoreImage
import CoreImage.CIFilterBuiltins

let text = CommandLine.arguments[1]
let out = CommandLine.arguments[2]

let filter = CIFilter.qrCodeGenerator()
filter.message = Data(text.utf8)
filter.correctionLevel = "M"
let scaled = filter.outputImage!.transformed(by: CGAffineTransform(scaleX: 16, y: 16))

let rep = NSCIImageRep(ciImage: scaled)
let image = NSImage(size: rep.size)
image.addRepresentation(rep)
let bitmap = NSBitmapImageRep(data: image.tiffRepresentation!)!
try! bitmap.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
print("wrote \(out)")
