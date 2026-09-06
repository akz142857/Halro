class Halro < Formula
  desc "Self-hosted LLM gateway for governance, routing, and accounting"
  homepage "https://halro.ai/"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/akz142857/Halro/releases/download/v0.7.0/halro-darwin-arm64.tar.gz"
      sha256 "1567edc773910043d4429cbff8475cac71a5c32297bc962d0355a631ebd53008"
    else
      url "https://github.com/akz142857/Halro/releases/download/v0.7.0/halro-darwin-amd64.tar.gz"
      sha256 "424fd6a2cbbe7fb6246df593f83b6d89b0ba57035fa0e94708ac50623a9e2b16"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/akz142857/Halro/releases/download/v0.7.0/halro-linux-arm64.tar.gz"
      sha256 "ff644f096178bbd65f636c2a2941f326488189d0bc53159542e2991c2cf3f516"
    else
      url "https://github.com/akz142857/Halro/releases/download/v0.7.0/halro-linux-amd64.tar.gz"
      sha256 "540b873a68429579506b46c75db1266b44e6746f60e67f4af4ce56bfe4295d7d"
    end
  end

  def install
    bin.install "halro", "halro-deadman"
    (pkgshare/"deadman").install "config.example.yaml", "config.schema.json", "event.schema.json"
    doc.install "README.md", "NOTICE", "THIRD_PARTY_NOTICES.md", "RECEIVER-CONTRACT.md"
  end

  def caveats
    <<~EOS
      Halro does not initialize configuration or start a service during install.
      Create and protect your configuration explicitly before starting Halro.
      The bundled dead-man example is installed under:
        #{pkgshare}/deadman
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/halro version")
    assert_predicate bin/"halro-deadman", :executable?
  end
end
