package main

import (
	"embed"
	"io/fs"

	"github.com/instantcocoa/devbox/cmd"
)

//go:embed terraform/configuration.nix
var embeddedNixConfig []byte

// The modules the main file imports. They are pushed together with it.
//
//go:embed all:terraform/devbox
var embeddedNixTree embed.FS

func main() {
	cmd.EmbeddedNixConfig = embeddedNixConfig
	// Root the tree at terraform/, so its entries are named "devbox/...".
	tree, err := fs.Sub(embeddedNixTree, "terraform")
	if err != nil {
		panic(err)
	}
	cmd.EmbeddedNixTree = tree
	cmd.Execute()
}
