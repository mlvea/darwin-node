# Sketch. Not published. Build notarized darwin/arm64 binaries first.
class DarwinNode < Formula
  desc "Kubernetes node agent for native macOS VMs on Apple Silicon"
  homepage "https://github.com/darwin-node/darwin-node"
  url "https://example.com/darwin-node-0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"

  depends_on :macos
  depends_on arch: :arm64

  def install
    bin.install "darwin-node"
    bin.install "darwin-guest-agent"
    bin.install "darwin-image"
    doc.install "LICENSE"
    doc.install "NOTICE"
    doc.install "THIRD_PARTY_NOTICES"
  end

  service do
    run [opt_bin/"darwin-node"]
    keep_alive true
    require_root true
  end

  test do
    system "#{bin}/darwin-node", "--help"
  end
end
