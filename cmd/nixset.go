package cmd

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A config set is what a push delivers to the box: the rendered main file,
// the devbox/ tree of modules beside it, and the operator's own local.nix
// when there is one. The box keeps each set in one directory and switches
// between sets with a single rename (see install_set.sh).

// nixTreeDir is the directory, beside the main file, that holds the modules
// the main file imports.
const nixTreeDir = "devbox"

// EmbeddedNixTree is the built-in copy of terraform/devbox, rooted so that its
// entries are named "devbox/...". Set once at startup by main.go, alongside
// EmbeddedNixConfig.
var EmbeddedNixTree fs.FS

// installSetScriptPath is the script, inside the tree, that installs a pushed
// set on the box. The CLI uploads the copy from the set it is pushing, so
// script and set always match; the box's boot-time restore service runs the
// same file.
const installSetScriptPath = nixTreeDir + "/install_set.sh"

// nixSource is the unrendered main file and the tree that belongs with it.
// Both always come from the same place.
type nixSource struct {
	main []byte
	// tree maps slash-separated paths below the set root ("devbox/...") to
	// file contents.
	tree map[string][]byte
}

// readNixSource reads the main file and its tree. A main file on disk takes
// its tree from the devbox/ directory beside it, and that directory must
// exist: mixing a main file from disk with the tree built into the binary
// would push a combination nobody wrote. Only when the default path is not on
// disk at all (an installed binary run outside a checkout) are both taken
// from the embedded copy.
func readNixSource(nixFile string) (nixSource, error) {
	main, err := os.ReadFile(nixFile)
	if err == nil {
		treeRoot := filepath.Dir(nixFile)
		tree, err := readTree(os.DirFS(treeRoot))
		if err != nil {
			return nixSource{}, fmt.Errorf("reading %s: %w", filepath.Join(treeRoot, nixTreeDir), err)
		}
		if len(tree) == 0 {
			return nixSource{}, fmt.Errorf("%s has no %s/ directory beside it; the config is pushed as a set and needs both", nixFile, nixTreeDir)
		}
		return nixSource{main: main, tree: tree}, nil
	}
	if os.IsNotExist(err) && nixFile == defaultNixFile() && len(EmbeddedNixConfig) > 0 {
		var tree map[string][]byte
		if EmbeddedNixTree != nil {
			if tree, err = readTree(EmbeddedNixTree); err != nil {
				return nixSource{}, fmt.Errorf("reading the built-in %s tree: %w", nixTreeDir, err)
			}
		}
		return nixSource{main: EmbeddedNixConfig, tree: tree}, nil
	}
	return nixSource{}, fmt.Errorf("reading %s: %w", nixFile, err)
}

// readTree returns every regular file under devbox/ in root. A root without
// that directory yields an empty tree.
func readTree(root fs.FS) (map[string][]byte, error) {
	tree := map[string][]byte{}
	err := fs.WalkDir(root, nixTreeDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			if entry.IsDir() {
				return nil
			}
			return fmt.Errorf("%s is not a regular file", name)
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		tree[name] = data
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil
		}
		return nil, err
	}
	return tree, nil
}

// readLocalModule returns the operator's local.nix, or nil when none is
// configured. A configured file that cannot be read is an error: pushing
// without it would silently drop the operator's settings.
func readLocalModule(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading nix_local_module %s: %w", path, err)
	}
	return data, nil
}

// buildSetTar packs a set for the box: configuration.nix (already rendered),
// the tree, and local.nix when given. Entries are sorted and carry no times
// or owners from this machine, so the same set always packs to the same
// bytes.
func buildSetTar(rendered []byte, tree map[string][]byte, localModule []byte) ([]byte, error) {
	files := map[string][]byte{"configuration.nix": rendered}
	for name, data := range tree {
		if path.Clean(name) != name || !strings.HasPrefix(name, nixTreeDir+"/") {
			return nil, fmt.Errorf("refusing to pack %q: not a clean path under %s/", name, nixTreeDir)
		}
		files[name] = data
	}
	if localModule != nil {
		files["local.nix"] = localModule
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var packed bytes.Buffer
	writer := tar.NewWriter(&packed)
	for _, name := range names {
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(files[name])),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return packed.Bytes(), nil
}

// newSetStamp names a set. Stamps sort by time, which is what the box relies
// on when it prunes old sets.
func newSetStamp(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}
