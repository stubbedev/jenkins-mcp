{
  description = "MCP server for Jenkins build status and logs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        # Version is read from package.json so a normal `npm version` release
        # bump propagates here automatically — the flake never needs hand-editing
        # on release. Only vendorHash changes, and only when Go deps change.
        version = (builtins.fromJSON (builtins.readFile ./package.json)).version;
        jenkins-mcp = pkgs.buildGoModule {
          pname = "jenkins-mcp";
          inherit version;
          # cleanSource keeps .git, result symlinks, and node_modules out of the
          # build sandbox; buildGoModule only needs the Go sources + go.mod/sum.
          src = pkgs.lib.cleanSource ./.;
          vendorHash = "sha256-muvvSH7mJBoPKp8NJyeAlQ5DiYdLy4bB4wcsaWFUyic=";
          # Version is embedded from package.json at compile time (see version.go),
          # so no -X ldflag is needed to set it.
          ldflags = [
            "-s"
            "-w"
          ];
          meta = with pkgs.lib; {
            description = "MCP server for Jenkins build status and logs";
            homepage = "https://github.com/stubbedev/jenkins-mcp";
            license = licenses.mit;
            mainProgram = "jenkins-mcp";
          };
        };
      in
      {
        packages.default = jenkins-mcp;
        packages.jenkins-mcp = jenkins-mcp;
        apps.default = flake-utils.lib.mkApp { drv = jenkins-mcp; };
        devShells.default = pkgs.mkShell { packages = [ pkgs.go ]; };
      }
    );
}
