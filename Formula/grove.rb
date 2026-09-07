# This file is generated and overwritten by goreleaser on every release (see .goreleaser.yaml's
# `brews` block) — the version/url/sha256 below are placeholders that only matter before the
# first `v*` tag is pushed. Do not hand-edit after that; a release run replaces this wholesale.
class Grove < Formula
  desc "Your Macs as a private build/agent cloud: Tart VMs via Orchard, jobs via Nomad."
  homepage "https://github.com/gm2211/grove"
  version "0.0.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/gm2211/grove/releases/download/v#{version}/grove_darwin_arm64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    else
      url "https://github.com/gm2211/grove/releases/download/v#{version}/grove_darwin_amd64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/gm2211/grove/releases/download/v#{version}/grove_linux_arm64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    else
      url "https://github.com/gm2211/grove/releases/download/v#{version}/grove_linux_amd64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  def install
    bin.install "grove"
  end

  test do
    system "#{bin}/grove", "--version"
  end
end
