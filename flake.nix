{
  description = "monokasa dev shell";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let pkgs = nixpkgs.legacyPackages.${system};
      in {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            golangci-lint
            gotools          
            gh
            git
            sqlite           
            cloudflared      
          ];

          shellHook = ''
            export GOFLAGS="-mod=mod"
            echo "monokasa dev shell — $(go version)"
            if [ ! -f .env ]; then
              echo "WARN: no .env (copy .env.example and fill in TG_TOKEN / TICKET_SECRET / MONO_JAR_LINK)"
            fi
          '';
        };
      });
}
