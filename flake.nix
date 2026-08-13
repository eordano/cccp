{
  description = "cccp - cloud code config provider";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));
    in
    {
      packages = forAll (pkgs: rec {
        cccp = pkgs.buildGoModule {
          pname = "cccp";
          version = "0.1.0";
          src = pkgs.lib.fileset.toSource {
            root = ./.;
            fileset = pkgs.lib.fileset.unions [
              ./cmd
              ./internal
              ./go.mod
              ./go.sum
              ./man
              ./completions
            ];
          };
          vendorHash = "sha256-bw8xkgO6KT0WkBfnejYm8FakVaPKlvBfMlDwLdRUzgo=";
          subPackages = [ "cmd/cccp" ];

          # Fetched rather than committed. It has to land before the build,
          # because server.go pulls it in with //go:embed public/*.
          preBuild = ''
            cp ${
              pkgs.fetchurl {
                url = "https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js";
                hash = "sha256-4gndpcgjVHnzFm3vx3UOHbzVpcGAi3eS/C5nM3aPtEc=";
              }
            } internal/pravda/public/htmx.min.js
            chmod +w internal/pravda/public/htmx.min.js
          '';
          ldflags = [
            "-s"
            "-w"
          ];
          doCheck = false;
          postInstall = ''
            install -Dm644 man/cccp.1           $out/share/man/man1/cccp.1
            install -Dm644 completions/cccp.bash $out/share/bash-completion/completions/cccp
            install -Dm644 completions/cccp.fish $out/share/fish/vendor_completions.d/cccp.fish
            install -Dm644 completions/_cccp    $out/share/zsh/site-functions/_cccp
          '';
          meta.description = "Sync ~/.claude across machines (serve / push / pull / watch / explore)";
          meta.mainProgram = "cccp";
        };
        default = cccp;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      });

      # nixosTest is Linux-only; gate the check so macOS eval still works.
      checks = forAll (
        pkgs:
        nixpkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
          e2e = import ./nix/tests/e2e.nix {
            inherit pkgs;
            cccpModule = self.nixosModules.default;
            cccpPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
          };
        }
      );

      nixosModules.default = import ./nix/module.nix;
    };
}
