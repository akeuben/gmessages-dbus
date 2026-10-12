{
    inputs = {
        nixpkgs.url = "github:nixos/nixpkgs/nixos-25.05";
        flake-parts.url = "github:hercules-ci/flake-parts";
        systems.url = "github:nix-systems/default";
    };

    outputs = inputs:
        inputs.flake-parts.lib.mkFlake { inherit inputs; } {
            systems = import inputs.systems;

            perSystem = { pkgs, lib, ... }: let
                java = pkgs.jetbrains.jdk-no-jcef;

                nativeBuildInputs = with pkgs; [
                    go
                ];

                buildInputs = with pkgs; [
                ];
            in {
                devShells.default = pkgs.mkShell {
                    inherit nativeBuildInputs buildInputs;

                    env = {
                        LD_LIBRARY_PATH = lib.makeLibraryPath buildInputs;
                        JAVA_HOME = "${java.home}";
                    };
                };
            };
        };
}
