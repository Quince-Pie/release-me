# The upstream Go toolchain, byte-for-byte as published on go.dev, pinned by
# SHA-256 for every build platform this flake supports.
#
# Why not nixpkgs' `go`: nixpkgs patches the standard library to embed the
# Nix store paths of tzdata, iana-etc and mailcap, and those paths differ per
# build platform, so binaries built with it can never be identical across
# x86_64 and aarch64 builders, nor reproduced by anyone using stock Go. With
# the upstream toolchain and -trimpath the output depends only on the source,
# the Go version and the build flags: anyone with go1.x.y can rebuild the
# release bytes without Nix at all (see docs/CONTRACT.md).
#
# The toolchain binaries are statically linked, so nothing is patched or
# stripped; nixpkgs itself bootstraps Go from these very tarballs.
# Update: bump `version` and refresh the hashes from
#   https://go.dev/dl/?mode=json&include=all
# and keep go.mod's `toolchain` line equal to it.
{
  lib,
  stdenvNoCC,
  fetchurl,
}:
let
  version = "1.27.1";
  dist = {
    x86_64-linux = {
      platform = "linux-amd64";
      sha256 = "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445";
    };
    aarch64-linux = {
      platform = "linux-arm64";
      sha256 = "3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec";
    };
    x86_64-darwin = {
      platform = "darwin-amd64";
      sha256 = "8f8f52c6649542cf027bbc9b9c68d1ec042f9f34808a40413f0b8b3f66f3caa4";
    };
    aarch64-darwin = {
      platform = "darwin-arm64";
      sha256 = "ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12";
    };
  };
  system = stdenvNoCC.buildPlatform.system;
  host =
    dist.${system}
      or (throw "go-toolchain: no upstream Go ${version} distribution pinned for ${system}");
in
stdenvNoCC.mkDerivation {
  pname = "go-upstream";
  inherit version;

  src = fetchurl {
    url = "https://go.dev/dl/go${version}.${host.platform}.tar.gz";
    inherit (host) sha256;
  };

  # Keep the published bytes exactly; there is nothing to fix up.
  dontConfigure = true;
  dontBuild = true;
  dontFixup = true;

  installPhase = ''
    runHook preInstall
    mkdir -p "$out/share/go" "$out/bin"
    cp -r . "$out/share/go"
    ln -s "$out/share/go/bin/go" "$out/bin/go"
    ln -s "$out/share/go/bin/gofmt" "$out/bin/gofmt"
    runHook postInstall
  '';

  passthru = {
    inherit (stdenvNoCC.hostPlatform.go) GOOS GOARCH;
    CGO_ENABLED = 0;
  };

  meta = {
    description = "Upstream Go ${version} toolchain, unpatched";
    homepage = "https://go.dev";
    license = lib.licenses.bsd3;
    platforms = builtins.attrNames dist;
  };
}
