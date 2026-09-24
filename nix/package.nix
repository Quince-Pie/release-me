# Builds release-me with the upstream Go toolchain in module mode, so that
# `go build` outside Nix with the same toolchain and flags produces the same
# bytes (a vendored build embeds different build information, see
# docs/CONTRACT.md). Deliberately a short mkDerivation rather than
# buildGoModule: the recipe that produces a release artifact should be
# auditable in one screen.
#
# `goos`/`goarch` select a foreign target (Go cross-compiles without a C
# toolchain because CGO is disabled); the native build additionally runs
# vet, the tests and the version self-check.
{
  lib,
  stdenvNoCC,
  buildGoModule,
  goToolchain,
  versionCheckHook,
  git,
  openssh,
  src,
  version,
  goos ? null,
  goarch ? null,
}:
let
  native = goos == null;
  exe = "release-me" + lib.optionalString (goos == "windows") ".exe";

  # The module download cache as a fixed-output derivation (nixpkgs'
  # proxyVendor machinery), served to `go build` as a file:// proxy so the
  # build runs in ordinary module mode without network access.
  goModules =
    (buildGoModule.override { go = goToolchain; } {
      pname = "release-me-modules";
      inherit version src;
      proxyVendor = true;
      vendorHash = "sha256-slht3YE66Yt7pWRaScTyMFQo+IvuroLYpnCgEJ+YlcY=";
    }).goModules;
in
stdenvNoCC.mkDerivation {
  pname = "release-me" + lib.optionalString (!native) "-${goos}-${goarch}";
  inherit version src;

  nativeBuildInputs = [ goToolchain ] ++ lib.optionals native [
    versionCheckHook
    git # gittag tests
    openssh # sshsig interoperability tests
  ];

  env = {
    CGO_ENABLED = "0";
    GO111MODULE = "on";
    GOTOOLCHAIN = "local";
    GOSUMDB = "off";
    GOPROXY = "file://${goModules}";
    # -trimpath removes build paths; -buildvcs=false keeps git metadata out
    # of the binary (the version comes from -X) so a checkout with or without
    # .git links identically; -mod=mod is the ordinary module mode.
    GOFLAGS = "-trimpath -buildvcs=false -mod=mod";
  }
  // lib.optionalAttrs (!native) {
    GOOS = goos;
    GOARCH = goarch;
  };

  # -buildid= makes the build ID empty instead of content-derived from
  # absolute paths; -s -w drop the symbol table and DWARF.
  ldflags = "-s -w -buildid= -X main.version=${version}";

  configurePhase = ''
    runHook preConfigure
    export HOME="$TMPDIR" GOCACHE="$TMPDIR/go-cache" GOPATH="$TMPDIR/go" GOMODCACHE="$TMPDIR/go/pkg/mod"
    runHook postConfigure
  '';

  buildPhase = ''
    runHook preBuild
    go build -ldflags "$ldflags" -o "${exe}" ./cmd/release-me
    runHook postBuild
  '';

  doCheck = native;
  checkPhase = ''
    runHook preCheck
    go vet ./...
    go test ./...
    runHook postCheck
  '';

  installPhase = ''
    runHook preInstall
    install -Dm755 "${exe}" "$out/bin/${exe}"
    runHook postInstall
  '';

  # The bytes the Go linker wrote are the release artifact. Nix's default
  # fixup would run strip/patchelf on native ELF binaries (rewriting the
  # section layout) but skip foreign ones, which alone makes builds differ
  # between build hosts.
  dontFixup = true;

  doInstallCheck = native;
  versionCheckProgramArg = "version";

  meta = {
    description = "Verifiable, reproducible release automation for GitHub and Forgejo-family hosts";
    homepage = "https://github.com/Quince-Pie/release-me";
    changelog = "https://github.com/Quince-Pie/release-me/blob/main/CHANGELOG.md";
    license = lib.licenses.asl20;
    mainProgram = "release-me";
    platforms = lib.platforms.unix ++ lib.platforms.windows;
  };
}
