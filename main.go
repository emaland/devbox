package main

import (
	_ "embed"

	"github.com/emaland/devbox/cmd"
)

//go:embed terraform/configuration.nix
var embeddedNixConfig []byte

func main() {
	cmd.EmbeddedNixConfig = embeddedNixConfig
	cmd.Execute()
}
