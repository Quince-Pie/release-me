{
  description = "release-me: verifiable, reproducible release automation for GitHub and Forgejo-family hosts";

  # nixos-unstable, pinned by flake.lock. Only development tooling and the
  # packaging derivations come from nixpkgs; the Go toolchain that produces
  # the release bytes is the upstream distribution pinned in nix/go-toolchain.nix.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/4975466d324710c576dc11ad614684e6bd8cad8e";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;

      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # CHANGELOG.md is the single source of truth for the version.
      versionInfo = import ./nix/version.nix { inherit lib self; };

      # Only Go inputs feed the build, so documentation edits do not rebuild it.
      goSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          ./cmd
          ./internal
        ];
      };

      # Release targets, named with Go's own GOOS/GOARCH vocabulary.
      targets = [
        { goos = "linux"; goarch = "amd64"; }
        { goos = "linux"; goarch = "arm64"; }
        { goos = "darwin"; goarch = "amd64"; }
        { goos = "darwin"; goarch = "arm64"; }
        { goos = "windows"; goarch = "amd64"; }
        { goos = "windows"; goarch = "arm64"; }
      ];

      packagesFor =
        pkgs:
        let
          goToolchain = pkgs.callPackage ./nix/go-toolchain.nix { };

          release-me = pkgs.callPackage ./nix/package.nix {
            inherit goToolchain;
            src = goSrc;
            inherit (versionInfo) version;
          };

          # Same recipe with a foreign GOOS/GOARCH; tests run in the native package.
          crossBinary =
            { goos, goarch }:
            pkgs.callPackage ./nix/package.nix {
              inherit goToolchain goos goarch;
              src = goSrc;
              inherit (versionInfo) version;
            };

          crossPackages = lib.listToAttrs (
            map (t: {
              name = "release-me-${t.goos}-${t.goarch}";
              value = crossBinary t;
            }) targets
          );

          release-assets = pkgs.callPackage ./nix/release-assets.nix {
            pname = "release-me";
            releaseMe = release-me;
            inherit (versionInfo) version epoch;
            license = ./LICENSE;
            binaries = map (t: t // { drv = crossBinary t; }) targets;
          };
        in
        {
          inherit release-me release-assets goToolchain;
          default = release-me;
        }
        // crossPackages;

      formatterFor =
        pkgs:
        let
          goToolchain = pkgs.callPackage ./nix/go-toolchain.nix { };
        in
        pkgs.nixfmt-tree.override {
          runtimeInputs = [
            goToolchain
            pkgs.shfmt
          ];
          settings = {
            formatter.gofmt = {
              command = "gofmt";
              options = [ "-w" ];
              includes = [ "*.go" ];
            };
            formatter.shfmt = {
              command = "shfmt";
              options = [ "-w" "-i" "2" "-ci" ];
              includes = [ "*.sh" ];
            };
          };
        };
    in
    {
      packages = forAllSystems packagesFor;

      formatter = forAllSystems formatterFor;

      checks = forAllSystems (
        pkgs:
        let
          p = packagesFor pkgs;
        in
        {
          # Builds the tool, runs vet and the tests, and packs the assets.
          inherit (p) release-me release-assets;

          go-lint =
            pkgs.runCommand "go-lint" { nativeBuildInputs = [ p.goToolchain ]; } ''
              cd ${goSrc}
              unformatted="$(gofmt -l .)"
              if [ -n "$unformatted" ]; then
                echo "gofmt: files need formatting:" "$unformatted" >&2
                exit 1
              fi
              touch "$out"
            '';

          changelog =
            pkgs.runCommand "changelog-lint" { nativeBuildInputs = [ p.release-me ]; } ''
              release-me changelog --file ${./CHANGELOG.md} lint
              touch "$out"
            '';

          shell-scripts =
            pkgs.runCommand "shell-scripts-lint"
              {
                nativeBuildInputs = [
                  pkgs.shellcheck
                  pkgs.shfmt
                ];
              }
              ''
                cd ${self}
                shellcheck --shell=bash --external-sources scripts/*.sh
                shfmt -d -i 2 -ci scripts
                touch "$out"
              '';

          workflows =
            pkgs.runCommand "workflows-lint"
              {
                nativeBuildInputs = [
                  pkgs.actionlint
                  pkgs.shellcheck
                  pkgs.zizmor
                ];
              }
              ''
                cd ${self}
                actionlint -color -config-file .github/actionlint.yaml .github/workflows/*.yml
                zizmor --offline --persona pedantic --min-severity low .github/workflows
                touch "$out"
              '';
        }
      );

      devShells = forAllSystems (
        pkgs:
        let
          p = packagesFor pkgs;
        in
        {
          default = pkgs.mkShell {
            packages =
              [ p.goToolchain ]
              ++ (with pkgs; [
                gopls
                gotools
                govulncheck
                golangci-lint
                git
                gh
                cosign
                forgejo
                forgejo-runner
                openssh
                jq
                curl
                shellcheck
                shfmt
                actionlint
                zizmor
                diffoscope
                syft
                nixfmt
              ])
              ++ [ (formatterFor pkgs) ];
            shellHook = ''
              export GOTOOLCHAIN=local
            '';
          };
        }
      );

      apps = forAllSystems (
        pkgs:
        let
          p = packagesFor pkgs;
        in
        {
          default = {
            type = "app";
            program = lib.getExe p.release-me;
            meta.description = "release-me";
          };
        }
      );

      lib = {
        inherit versionInfo;
      };
    };
}
