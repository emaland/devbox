# The AHP gateway, built from source with Node 24.
#
# The defaults build the public release. Another build is an override, for
# example in the operator's local.nix:
#
#   services.ahp-gateway.package = pkgs.callPackage ./devbox/pkgs/ahp-acp-gateway.nix {
#     version = "...";
#     source = ...;        # a path or a fetcher, with its hash
#     npmDepsHash = "sha256-...";
#   };
#
# The argument is `source`, not `src`: callPackage would fill an argument
# named `src` from an unrelated nixpkgs attribute.
{ lib
, buildNpmPackage
, fetchFromGitHub
, nodejs_24
, version ? "0.2.2"
, source ? fetchFromGitHub {
    owner = "formulahendry";
    repo = "ahp-acp-gateway";
    rev = "v0.2.2";
    hash = "sha256-x0ixf0ozIfchHFMJHJVi7UO9SbRaiS2j2DdLQlrWRWw=";
  }
, npmDepsHash ? "sha256-Ug4NurWdj8oveOC7iUyuJdg9PN/NXbnbqB6SCBcKg2Q="
}:

buildNpmPackage {
  pname = "ahp-acp-gateway";
  inherit version npmDepsHash;
  src = source;
  nodejs = nodejs_24;
  npmBuildScript = "build";
  # The gateway's own test suite runs in its own repository.
  doCheck = false;
  meta = {
    description = "AHP server that connects Agent Host Protocol clients to ACP agents";
    homepage = "https://github.com/formulahendry/ahp-acp-gateway";
    license = lib.licenses.mit;
    mainProgram = "ahp-acp-gateway";
  };
}
