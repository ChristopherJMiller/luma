{
  description = "luma: a household agent platform (Go Core, TypeScript conductor, assistant-ui PWA) - dev toolchain";

  # nixos-unstable, pinned by flake.lock. Bump with `nix flake update nixpkgs`,
  # then run `nix develop -c make tools-versions` and `make test` before
  # committing the new lock. Go and Node are pinned to a minor (go_1_26,
  # nodejs_24) so a nixpkgs bump cannot silently move the language versions;
  # hack/check-versions.sh holds go.mod and the Dockerfile to the same minors.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (
        pkgs:
        let
          # GO-2026-6809 (crypto/x509) fixed in 1.26.10; drop this override
          # when nixos-unstable ships go >= 1.26.10. Until then the shell
          # builds 1.26.10 from source; once nixpkgs' go_1_26 reaches it, the
          # version check below falls through to nixpkgs' own, so the
          # override retires itself. The let binding shadows `with pkgs;`
          # below, so the list's go_1_26 is this one.
          go_1_26 =
            if pkgs.lib.versionAtLeast pkgs.go_1_26.version "1.26.10" then
              pkgs.go_1_26
            else
              pkgs.go_1_26.overrideAttrs (_: {
                version = "1.26.10";
                src = pkgs.fetchurl {
                  url = "https://go.dev/dl/go1.26.10.src.tar.gz";
                  hash = "sha256-/WPog7ovK/ypPBZGsx6+9j4rHk0rMXyXidl2po2B828=";
                };
              });
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              # Go
              go_1_26
              gopls
              gotools
              golangci-lint
              govulncheck # make vulncheck

              # TypeScript
              nodejs_24
              pnpm_10

              # Tests
              postgresql_16 # a throwaway server for the store tests
              libfaketime # Spike S: clock control for the CLI subprocess

              # Kubernetes / e2e
              kind
              kubectl
              kubernetes-helm
              docker-client
              # `docker buildx`: `imagetools inspect` for pinned digests (Task 3)
              # and multi-arch image builds (Task 25). docker-client already loads
              # this same package as its CLI plugin; it is listed so the dependency
              # is explicit, and `make tools-versions` fails if the plugin goes.
              docker-buildx

              # Plumbing
              gnumake
              git
              jq
              yq-go
              actionlint
              gitleaks # make check-hygiene: no secrets in the public history
              openssl
            ];

            # Never let `go` fetch a different toolchain behind the flake's back.
            GOTOOLCHAIN = "local";

            shellHook = ''
              # e2e kind clusters keep their kubeconfig here, never in ~/.kube/config.
              export LUMA_E2E_KUBECONFIG="$(git rev-parse --show-toplevel 2>/dev/null || pwd)/.e2e/kubeconfig"
              # libfaketime's preload library, for Spike S.
              export LUMA_FAKETIME_LIB="${pkgs.libfaketime}/lib/libfaketime.so.1"
            '';
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
