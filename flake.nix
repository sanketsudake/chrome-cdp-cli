{
  description = "Drive your already-running local Chrome from the command line over CDP";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # Keep in step with the release tag; goreleaser stamps the same string
      # into internal/cli from {{ .Version }}.
      version = "0.3.2";

      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        chrome-cdp = pkgs.buildGoModule {
          pname = "chrome-cdp";
          inherit version;
          src = self;

          vendorHash = "sha256-8f/eoBJZxQHsiUpUbbbw9NWiduRT+NCEEu90HVWN9vE=";

          subPackages = [ "cmd/chrome-cdp" ];

          # Matches the goreleaser build: pure Go, no cgo.
          env.CGO_ENABLED = 0;

          ldflags = [
            "-s"
            "-w"
            "-X github.com/sanketsudake/chrome-cdp-cli/internal/cli.Version=${version}"
          ];

          meta = {
            description = "Drive your already-running local Chrome from the command line over CDP";
            homepage = "https://github.com/sanketsudake/chrome-cdp-cli";
            license = pkgs.lib.licenses.mit;
            mainProgram = "chrome-cdp";
          };
        };
        default = chrome-cdp;
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
