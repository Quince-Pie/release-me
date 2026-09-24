# Packs the cross-compiled binaries into the archives that are published as
# release assets, plus the SHA256SUMS manifest, using release-me's own
# reproducible packer. The archive bytes depend only on the binaries, the
# LICENSE, the member names and the commit timestamp.
{
  lib,
  stdenvNoCC,
  releaseMe, # the native package, for `release-me pack` and `manifest`
  pname,
  version,
  epoch,
  license,
  binaries, # list of { goos; goarch; drv; }
}:
stdenvNoCC.mkDerivation {
  name = "${pname}-release-assets-${version}";
  nativeBuildInputs = [ releaseMe ];
  dontUnpack = true;
  buildCommand = ''
    mkdir -p "$out"
    ${lib.concatMapStringsSep "\n" (
      b:
      let
        exe = if b.goos == "windows" then "${pname}.exe" else pname;
        ext = if b.goos == "windows" then "zip" else "tar.gz";
      in
      ''
        release-me pack --out "$out/${pname}_${version}_${b.goos}_${b.goarch}.${ext}" --mtime ${toString epoch} \
          "${exe}=${b.drv}/bin/${exe}" "LICENSE=${license}"
      ''
    ) binaries}
    release-me manifest create "$out"
    release-me manifest check "$out"
  '';
}
