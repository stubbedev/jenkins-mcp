{
  perSystem =
    {
      lib,
      pkgs,
      self',
      ...
    }:
    {
      packages = {
        jenkins-mcp = pkgs.buildGo127Module {
          pname = "jenkins-mcp";
          # Version is read from package.json so a normal `npm version` release
          # bump propagates here automatically — the flake never needs hand-editing
          # on release. Only vendorHash changes, and only when Go deps change.
          inherit (lib.importJSON ../package.json) version;
          # Only tracked files reach the build sandbox, minus the nix files, so
          # editing those does not rebuild the package.
          src = lib.fileset.toSource {
            root = ../.;
            fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
              lib.fileset.unions [
                ../flake.nix
                ../flake.lock
                ../nix
              ]
            );
          };
          vendorHash = "sha256-vlB5cmNGjEu9J68qzb/WosXziJjMfE7of+gsp1RwO/I=";
          # Version is embedded from package.json at compile time (see version.go),
          # so no -X ldflag is needed to set it.
          ldflags = [
            "-s"
            "-w"
          ];
          meta = {
            description = "MCP server for Jenkins build status and logs";
            homepage = "https://github.com/stubbedev/jenkins-mcp";
            license = lib.licenses.mit;
            mainProgram = "jenkins-mcp";
          };
        };
        default = self'.packages.jenkins-mcp;
      };

      apps.default = {
        type = "app";
        program = lib.getExe self'.packages.jenkins-mcp;
      };
    };
}
