package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/instantcocoa/devbox/internal/config"
)

// realInstallScript is the script the CLI uploads and the box's restore
// service runs.
func realInstallScript() string {
	return filepath.Join("..", "terraform", filepath.FromSlash(installSetScriptPath))
}

// box is a stand-in for the two directories the install script works on.
type box struct {
	t      *testing.T
	config string // /etc/nixos on a real box
	saved  string // ~/.config/devbox on the persistent volume
}

func newBox(t *testing.T) box {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	root := t.TempDir()
	b := box{t: t, config: filepath.Join(root, "etc-nixos"), saved: filepath.Join(root, "saved")}
	for _, dir := range []string{b.config, b.saved} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// run executes the install script and returns its combined output and exit
// status.
func (b box) run(args ...string) (string, int) {
	b.t.Helper()
	return b.runEnv(nil, args...)
}

// runEnv is run with extra environment variables ("NAME=value").
func (b box) runEnv(env []string, args ...string) (string, int) {
	b.t.Helper()
	command := exec.Command("sh", append([]string{realInstallScript()}, args...)...)
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		b.t.Fatalf("running the install script: %v", err)
		return "", -1
	}
}

// push packs a set and installs it, as a push does.
func (b box) push(stamp, main, layout string, localModule []byte) (string, int) {
	b.t.Helper()
	return b.pushEnv(nil, stamp, main, layout, localModule)
}

func (b box) pushEnv(env []string, stamp, main, layout string, localModule []byte) (string, int) {
	b.t.Helper()
	tree := map[string][]byte{"devbox/modules/example.nix": []byte("{ }\n")}
	if layout != "" {
		tree["devbox/layout-version"] = []byte(layout + "\n")
	}
	packed, err := buildSetTar([]byte(main), tree, localModule)
	if err != nil {
		b.t.Fatal(err)
	}
	tarball := filepath.Join(b.t.TempDir(), "set.tar")
	if err := os.WriteFile(tarball, packed, 0o600); err != nil {
		b.t.Fatal(err)
	}
	return b.runEnv(env, "install", tarball, b.config, b.saved, stamp)
}

func (b box) mustPush(stamp, main, layout string, localModule []byte) {
	b.t.Helper()
	if out, status := b.push(stamp, main, layout, localModule); status != 0 {
		b.t.Fatalf("install of %s exited %d:\n%s", stamp, status, out)
	}
}

func (b box) mustSave() {
	b.t.Helper()
	if out, status := b.run("save", b.config, b.saved); status != 0 {
		b.t.Fatalf("save exited %d:\n%s", status, out)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func setsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "sets"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestInstallSetReplacesTheSingleFileLayout(t *testing.T) {
	b := newBox(t)
	// A box from before sets: one regular, read-only file.
	if err := os.WriteFile(filepath.Join(b.config, "configuration.nix"), []byte("old single file\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	b.mustPush("20260101T000000Z", "first set\n", "1", nil)

	main := filepath.Join(b.config, "configuration.nix")
	if got := read(t, main); got != "first set\n" {
		t.Errorf("configuration.nix reads %q, want the pushed set's main file", got)
	}
	if target, err := os.Readlink(main); err != nil || target != "current/configuration.nix" {
		t.Errorf("configuration.nix should be the fixed link current/configuration.nix, got %q, %v", target, err)
	}
	if target, err := os.Readlink(filepath.Join(b.config, "current")); err != nil || target != "sets/20260101T000000Z" {
		t.Errorf("current -> %q, %v", target, err)
	}
	if !exists(filepath.Join(b.config, "current", "devbox", "modules", "example.nix")) {
		t.Error("the tree was not installed beside the main file")
	}
}

func TestInstallSetSwitchesAndKeepsOnlyThePreviousSet(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "one\n", "1", nil)
	b.mustPush("20260102T000000Z", "two\n", "1", nil)
	b.mustPush("20260103T000000Z", "three\n", "1", nil)

	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "three\n" {
		t.Errorf("in use: %q, want the last push", got)
	}
	got := strings.Join(setsIn(t, b.config), " ")
	if want := "20260102T000000Z 20260103T000000Z"; got != want {
		t.Errorf("sets kept: %q, want %q (the one in use and the previous one)", got, want)
	}
}

func TestInstallSetRefusesAnOlderLayout(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "new layout\n", "2", nil)

	for i, layout := range []string{"1", ""} {
		t.Run("layout "+strconv.Quote(layout), func(t *testing.T) {
			out, status := b.push(fmt.Sprintf("2026010%dT000000Z", i+2), "from an old CLI\n", layout, nil)
			if status != 3 {
				t.Fatalf("exit %d, want 3:\n%s", status, out)
			}
			if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "new layout\n" {
				t.Errorf("the set in use changed to %q", got)
			}
			if got := strings.Join(setsIn(t, b.config), " "); got != "20260101T000000Z" {
				t.Errorf("a refused push left something behind: %q", got)
			}
		})
	}

	// The same or a newer layout is accepted.
	b.mustPush("20260103T000000Z", "same layout\n", "2", nil)
}

func TestInstallSetCarriesLocalModuleForward(t *testing.T) {
	t.Run("pushed with the set", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator one\n"))
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator one\n" {
			t.Errorf("local.nix = %q", got)
		}
	})

	t.Run("not pushed: taken from the set in use", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator one\n"))
		b.mustPush("20260102T000000Z", "main two\n", "1", nil)
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator one\n" {
			t.Errorf("local.nix was not carried forward: %q", got)
		}
	})

	t.Run("a pushed one replaces the old one", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator one\n"))
		b.mustPush("20260102T000000Z", "main two\n", "1", []byte("operator two\n"))
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator two\n" {
			t.Errorf("local.nix = %q", got)
		}
	})

	t.Run("fresh root volume: taken from the saved copy", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator one\n"))
		b.mustSave()
		// The instance is replaced: /etc/nixos is new, the saved copy survives.
		if err := os.RemoveAll(b.config); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(b.config, 0o755); err != nil {
			t.Fatal(err)
		}
		b.mustPush("20260102T000000Z", "main two\n", "1", nil)
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator one\n" {
			t.Errorf("local.nix was not restored from the saved copy: %q", got)
		}
	})

	t.Run("expected but nowhere to be found: nothing is switched", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator one\n"))
		b.mustSave()
		// Fresh root volume and the saved set lost its local.nix.
		if err := os.RemoveAll(b.config); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(b.config, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(b.saved, "current", "local.nix")); err != nil {
			t.Fatal(err)
		}
		out, status := b.push("20260102T000000Z", "main two\n", "1", nil)
		if status != 4 {
			t.Fatalf("exit %d, want 4:\n%s", status, out)
		}
		if !strings.Contains(out, "nix_local_module") {
			t.Errorf("the message should name the config key to set: %s", out)
		}
		if exists(filepath.Join(b.config, "current")) || exists(filepath.Join(b.config, "configuration.nix")) {
			t.Error("a set was switched to although the operator module is missing")
		}
	})

	t.Run("never had one: nothing is required", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", nil)
		b.mustSave()
		b.mustPush("20260102T000000Z", "main two\n", "1", nil)
		if exists(filepath.Join(b.config, "current", "local.nix")) {
			t.Error("a local.nix appeared from nowhere")
		}
		if exists(filepath.Join(b.saved, "local-module-expected")) {
			t.Error("the saved copy claims a local.nix is expected")
		}
	})
}

func TestInstallSetSurvivesAnInterruptedPush(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "good\n", "1", nil)

	// Leftovers of a push that died half-way, at each point it can die.
	if err := os.MkdirAll(filepath.Join(b.config, "sets", "20260102T000000Z.partial", "devbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sets/20260102T000000Z", filepath.Join(b.config, "current.new")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "good\n" {
		t.Fatalf("leftovers alone changed the set in use: %q", got)
	}

	t.Run("a broken archive changes nothing", func(t *testing.T) {
		tarball := filepath.Join(t.TempDir(), "set.tar")
		if err := os.WriteFile(tarball, []byte("this is not a tar archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, status := b.run("install", tarball, b.config, b.saved, "20260103T000000Z")
		if status == 0 {
			t.Fatalf("a broken archive was accepted:\n%s", out)
		}
		if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "good\n" {
			t.Errorf("the set in use changed: %q", got)
		}
	})

	t.Run("the next push works and clears the leftovers", func(t *testing.T) {
		b.mustPush("20260104T000000Z", "after\n", "1", nil)
		if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "after\n" {
			t.Errorf("in use: %q", got)
		}
		for _, name := range setsIn(t, b.config) {
			if strings.HasSuffix(name, ".partial") {
				t.Errorf("half-written set %s was not cleared", name)
			}
		}
		if exists(filepath.Join(b.config, "current.new")) {
			t.Error("stale current.new was not cleared")
		}
	})
}

func TestInstallSetRejectsBadInput(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "good\n", "1", nil)

	for _, stamp := range []string{"", "..", "a/b", ".hidden", "with space"} {
		if out, status := b.push(stamp, "x\n", "1", nil); status == 0 {
			t.Errorf("stamp %q was accepted:\n%s", stamp, out)
		}
	}
	if out, status := b.push("20260101T000000Z", "again\n", "1", nil); status == 0 {
		t.Errorf("an existing set was overwritten:\n%s", out)
	}
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "good\n" {
		t.Errorf("the set in use changed: %q", got)
	}
}

func TestSaveSetMirrorsTheSetInUse(t *testing.T) {
	b := newBox(t)
	// An older saved copy is a single read-only file at this path.
	legacy := filepath.Join(b.saved, "configuration.nix")
	if err := os.WriteFile(legacy, []byte("old saved copy\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	b.mustPush("20260101T000000Z", "one\n", "1", []byte("operator\n"))
	b.mustSave()

	// The old path still reads as the config, for a system that still runs
	// the old single-file restore service.
	if got := read(t, legacy); got != "one\n" {
		t.Errorf("saved configuration.nix reads %q", got)
	}
	if got := read(t, filepath.Join(b.saved, "current", "local.nix")); got != "operator\n" {
		t.Errorf("saved local.nix = %q", got)
	}
	if !exists(filepath.Join(b.saved, "local-module-expected")) {
		t.Error("the saved copy should record that a local.nix is expected")
	}

	// Saving twice is harmless; a later set replaces it and the marker follows
	// the set.
	b.mustSave()
	b.mustPush("20260102T000000Z", "two\n", "1", nil)
	if err := os.Remove(filepath.Join(b.config, "current", "local.nix")); err != nil {
		t.Fatal(err)
	}
	b.mustSave()
	if got := read(t, legacy); got != "two\n" {
		t.Errorf("saved configuration.nix reads %q after the second push", got)
	}
	if exists(filepath.Join(b.saved, "local-module-expected")) {
		t.Error("the marker should go when the set has no local.nix")
	}
}

func TestRestoreSet(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "one\n", "1", []byte("operator\n"))
	b.mustSave()

	t.Run("same set: nothing to do", func(t *testing.T) {
		out, status := b.run("restore", b.config, b.saved)
		if status != 0 || !strings.Contains(out, "already matches") {
			t.Fatalf("exit %d: %s", status, out)
		}
	})

	t.Run("the set in use was edited by hand: the saved one comes back", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(b.config, "current", "configuration.nix"), []byte("hand edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, status := b.run("restore", b.config, b.saved); status != 0 {
			t.Fatalf("exit %d: %s", status, out)
		}
		if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "one\n" {
			t.Errorf("in use: %q", got)
		}
	})

	t.Run("fresh root volume: the saved set is installed", func(t *testing.T) {
		if err := os.RemoveAll(b.config); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(b.config, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, status := b.run("restore", b.config, b.saved); status != 0 {
			t.Fatalf("exit %d: %s", status, out)
		}
		if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "one\n" {
			t.Errorf("in use: %q", got)
		}
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator\n" {
			t.Errorf("local.nix = %q", got)
		}
	})

	t.Run("no saved set: an error, and nothing touched", func(t *testing.T) {
		empty := newBox(t)
		if _, status := empty.run("restore", empty.config, empty.saved); status == 0 {
			t.Error("restore with no saved set should fail")
		}
	})
}

// Nix resolves a relative import against the real directory of the importing
// file. The layout depends on that: the main file is reached through two
// symlinks and must still find the tree and local.nix of its own set.
func TestInstalledSetEvaluatesThroughItsLinks(t *testing.T) {
	if _, err := exec.LookPath("nix-instantiate"); err != nil {
		t.Skip("nix-instantiate not on PATH")
	}
	b := newBox(t)
	main := `{ module = import ./devbox/modules/example.nix; local = import ./local.nix; }` + "\n"
	b.mustPush("20260101T000000Z", main, "1", []byte(`"first operator module"`+"\n"))
	b.mustPush("20260102T000000Z", main, "1", []byte(`"second operator module"`+"\n"))

	out, err := exec.Command("nix-instantiate", "--eval", "--strict", "--json",
		filepath.Join(b.config, "configuration.nix"), "-A", "local").Output()
	if err != nil {
		t.Fatalf("evaluating through the links: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != `"second operator module"` {
		t.Errorf("evaluated local.nix of the wrong set: %s", got)
	}
}

func TestBuildSetTar(t *testing.T) {
	tree := map[string][]byte{
		"devbox/modules/b.nix":  []byte("b"),
		"devbox/layout-version": []byte("1\n"),
		"devbox/modules/a.nix":  []byte("a"),
	}
	packed, err := buildSetTar([]byte("main"), tree, []byte("local"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := buildSetTar([]byte("main"), tree, []byte("local"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packed, again) {
		t.Error("the same set packed to different bytes")
	}

	var names []string
	reader := tar.NewReader(bytes.NewReader(packed))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 0 || header.Uname != "" || header.Mode != 0o644 {
			t.Errorf("%s carries owner or mode from this machine: uid %d %q mode %o", header.Name, header.Uid, header.Uname, header.Mode)
		}
		names = append(names, header.Name)
	}
	want := "configuration.nix devbox/layout-version devbox/modules/a.nix devbox/modules/b.nix local.nix"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("entries %q, want %q", got, want)
	}

	if without, err := buildSetTar([]byte("main"), tree, nil); err != nil || bytes.Contains(without, []byte("local.nix")) {
		t.Errorf("no local module given, but the archive has one (err %v)", err)
	}

	for _, bad := range []string{"../escape.nix", "/abs.nix", "devbox/../../escape.nix", "other/file.nix", "devbox//x.nix"} {
		if _, err := buildSetTar([]byte("main"), map[string][]byte{bad: []byte("x")}, nil); err == nil {
			t.Errorf("path %q was packed", bad)
		}
	}
}

func TestReadNixSource(t *testing.T) {
	t.Run("from disk, with the tree beside the main file", func(t *testing.T) {
		source, err := readNixSource(writeTemplate(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := source.tree["devbox/layout-version"]; !ok || len(source.main) == 0 {
			t.Errorf("tree %v", source.tree)
		}
	})

	t.Run("a main file with no tree beside it is an error, not a mix with the built-in tree", func(t *testing.T) {
		alone := filepath.Join(t.TempDir(), "configuration.nix")
		if err := os.WriteFile(alone, []byte("{ }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readNixSource(alone); err == nil || !strings.Contains(err.Error(), "devbox/") {
			t.Fatalf("want an error naming the missing tree, got %v", err)
		}
	})

	t.Run("built-in copy when the default path is not on disk", func(t *testing.T) {
		keepConfig, keepTree := EmbeddedNixConfig, EmbeddedNixTree
		t.Cleanup(func() { EmbeddedNixConfig, EmbeddedNixTree = keepConfig, keepTree })
		EmbeddedNixConfig = []byte("built-in main\n")
		EmbeddedNixTree = fstest.MapFS{"devbox/layout-version": {Data: []byte("7\n")}}

		// Tests run in cmd/, where the default relative path does not exist.
		source, err := readNixSource(defaultNixFile())
		if err != nil {
			t.Fatal(err)
		}
		if string(source.main) != "built-in main\n" || string(source.tree["devbox/layout-version"]) != "7\n" {
			t.Errorf("got main %q tree %v", source.main, source.tree)
		}
	})
}

func TestSetForPush(t *testing.T) {
	ctx := context.Background()
	found := func(context.Context, string) (string, error) { return "/hostedzone/ZLOOKEDUP", nil }
	localModule := filepath.Join(t.TempDir(), "local.nix")
	if err := os.WriteFile(localModule, []byte("operator module\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dcfg := config.DevboxConfig{DNSZone: "example.com.", DNSName: "box.example.com", NixLocalModule: localModule}
	packed, script, err := setForPush(ctx, dcfg, writeTemplate(t), "", found)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(script, []byte("install_set.sh install")) {
		t.Error("the install script returned is not the one from the tree")
	}

	files := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(packed))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(data)
	}
	if !strings.Contains(files["configuration.nix"], `zone = "ZLOOKEDUP";`) {
		t.Errorf("the packed main file is not rendered:\n%s", files["configuration.nix"])
	}
	if files["local.nix"] != "operator module\n" {
		t.Errorf("local.nix = %q", files["local.nix"])
	}
	if _, ok := files[installSetScriptPath]; !ok {
		t.Error("the tree is not in the archive")
	}

	t.Run("a configured local module that cannot be read stops the push", func(t *testing.T) {
		dcfg.NixLocalModule = filepath.Join(t.TempDir(), "missing.nix")
		if _, _, err := setForPush(ctx, dcfg, writeTemplate(t), "", found); err == nil {
			t.Fatal("expected an error")
		}
		if _, err := preflightNixConfig(ctx, dcfg, writeTemplate(t), found); err == nil {
			t.Fatal("the preflight should catch it before any instance work")
		}
	})
}

// ---- The saved copy is owned by the user; the script runs as root. ----

// fresh replaces /etc/nixos with an empty directory, as an instance
// replacement does. The saved copy on /home survives.
func (b box) fresh() {
	b.t.Helper()
	if err := os.RemoveAll(b.config); err != nil {
		b.t.Fatal(err)
	}
	if err := os.MkdirAll(b.config, 0o755); err != nil {
		b.t.Fatal(err)
	}
}

func TestCarryForwardDoesNotFollowLinksInTheSavedCopy(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator\n"))
	b.mustSave()
	b.fresh()

	secret := filepath.Join(t.TempDir(), "readable-only-by-root")
	if err := os.WriteFile(secret, []byte("not for the box\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	savedLocal := filepath.Join(b.saved, "current", "local.nix")
	if err := os.Remove(savedLocal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, savedLocal); err != nil {
		t.Fatal(err)
	}

	out, status := b.push("20260102T000000Z", "main two\n", "1", nil)
	if status == 0 {
		t.Errorf("a saved local.nix that is a link was accepted:\n%s", out)
	}
	if data, err := os.ReadFile(filepath.Join(b.config, "current", "local.nix")); err == nil && strings.Contains(string(data), "not for the box") {
		t.Error("the link target was copied into the config")
	}
}

func TestSaveDoesNotWriteThroughALinkedMarker(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator\n"))
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(b.saved, "local-module-expected")); err != nil {
		t.Fatal(err)
	}
	b.mustSave()
	if got := read(t, victim); got != "keep me\n" {
		t.Errorf("save wrote through the marker link; the target now reads %q", got)
	}
}

func TestRestoreRefusesAnUntrustworthySavedCopy(t *testing.T) {
	setUp := func(t *testing.T) box {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "in use\n", "1", nil)
		b.mustSave()
		return b
	}
	inUseSurvives := func(t *testing.T, b box) {
		t.Helper()
		if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "in use\n" {
			t.Errorf("the set in use was changed or lost: %q", got)
		}
		if _, err := os.Lstat(filepath.Join(b.config, "sets", "20260101T000000Z")); err != nil {
			t.Errorf("the set in use was deleted: %v", err)
		}
	}
	elsewhere := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(filepath.Join(dir, "devbox"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "configuration.nix"), []byte("from elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	relink := func(t *testing.T, b box, target string) {
		t.Helper()
		current := filepath.Join(b.saved, "current")
		if err := os.Remove(current); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, current); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		break_ func(t *testing.T, b box)
	}{
		{"the saved set is a link to another directory", func(t *testing.T, b box) {
			set := filepath.Join(b.saved, "sets", "20260105T000000Z")
			if err := os.Symlink(elsewhere(t), set); err != nil {
				t.Fatal(err)
			}
			relink(t, b, "sets/20260105T000000Z")
		}},
		{"current points outside sets/", func(t *testing.T, b box) {
			relink(t, b, elsewhere(t))
		}},
		{"the saved set holds a link", func(t *testing.T, b box) {
			src := filepath.Join(b.saved, "sets", "20260101T000000Z")
			dst := filepath.Join(b.saved, "sets", "20260105T000000Z")
			if err := exec.Command("cp", "-R", src, dst).Run(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, "configuration.nix"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/", filepath.Join(dst, "devbox", "root")); err != nil {
				t.Fatal(err)
			}
			relink(t, b, "sets/20260105T000000Z")
		}},
		{"the saved set has no main file", func(t *testing.T, b box) {
			dst := filepath.Join(b.saved, "sets", "20260105T000000Z")
			if err := os.MkdirAll(filepath.Join(dst, "devbox"), 0o755); err != nil {
				t.Fatal(err)
			}
			relink(t, b, "sets/20260105T000000Z")
		}},
		{"a set name with a newline", func(t *testing.T, b box) {
			src := filepath.Join(b.saved, "sets", "20260101T000000Z")
			dst := filepath.Join(b.saved, "sets", "\nb")
			if err := exec.Command("cp", "-R", src, dst).Run(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, "configuration.nix"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			relink(t, b, "sets/\nb")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := setUp(t)
			tc.break_(t, b)
			out, status := b.run("restore", b.config, b.saved)
			if status == 0 {
				t.Errorf("restore accepted it:\n%s", out)
			}
			inUseSurvives(t, b)
		})
	}
}

func TestPruneIgnoresNamesItDidNotMake(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "one\n", "1", nil)
	odd := filepath.Join(b.config, "sets", "\nb")
	if err := os.MkdirAll(odd, 0o755); err != nil {
		t.Fatal(err)
	}
	b.mustPush("20260102T000000Z", "two\n", "1", nil)
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "two\n" {
		t.Fatalf("in use: %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.config, "sets", "20260101T000000Z")); err != nil {
		t.Errorf("the previous set was pruned: %v", err)
	}
}

func TestInstallWhenTheSavedCopyCannotBeSeen(t *testing.T) {
	t.Run("home volume exists but is not mounted, nothing on the box: stop", func(t *testing.T) {
		b := newBox(t)
		out, status := b.pushEnv([]string{"DEVBOX_SAVED_STATE=unmounted"}, "20260101T000000Z", "main\n", "1", nil)
		if status != 4 {
			t.Fatalf("exit %d, want 4:\n%s", status, out)
		}
		if exists(filepath.Join(b.config, "current")) {
			t.Error("a set was switched to")
		}
	})
	t.Run("home volume exists but is not mounted, local.nix on the box: go on", func(t *testing.T) {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "main\n", "1", []byte("operator\n"))
		out, status := b.pushEnv([]string{"DEVBOX_SAVED_STATE=unmounted"}, "20260102T000000Z", "main two\n", "1", nil)
		if status != 0 {
			t.Fatalf("exit %d:\n%s", status, out)
		}
		if got := read(t, filepath.Join(b.config, "current", "local.nix")); got != "operator\n" {
			t.Errorf("local.nix = %q", got)
		}
	})
	t.Run("a brand-new blank volume has no saved copy at all: go on", func(t *testing.T) {
		b := newBox(t)
		out, status := b.pushEnv([]string{"DEVBOX_SAVED_STATE=none"}, "20260101T000000Z", "main\n", "1", nil)
		if status != 0 {
			t.Fatalf("exit %d:\n%s", status, out)
		}
	})
	t.Run("an unknown state is an error", func(t *testing.T) {
		b := newBox(t)
		if _, status := b.pushEnv([]string{"DEVBOX_SAVED_STATE=maybe"}, "20260101T000000Z", "main\n", "1", nil); status == 0 {
			t.Error("accepted")
		}
	})
}

func TestInstallChecksTheLayoutOfTheSavedSetOnAFreshRoot(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "new layout\n", "2", nil)
	b.mustSave()
	b.fresh()
	out, status := b.push("20260102T000000Z", "from an old CLI\n", "1", nil)
	if status != 3 {
		t.Fatalf("exit %d, want 3:\n%s", status, out)
	}
}

func TestSaveRefreshesAChangedSavedSet(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "one\n", "1", nil)
	b.mustSave()
	// Same name, different content in the saved copy (for example a restore
	// that was interrupted, or a hand edit).
	if err := os.WriteFile(filepath.Join(b.saved, "current", "configuration.nix"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.mustSave()
	if got := read(t, filepath.Join(b.saved, "configuration.nix")); got != "one\n" {
		t.Errorf("the saved copy was not refreshed: %q", got)
	}
}

// The boot-time restore service lives in the template. Run its script with
// the box's paths pointed at temporary directories.
func runRestoreService(t *testing.T, b box) string {
	t.Helper()
	data, err := os.ReadFile(realTemplate())
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?s)writeShellScript "devbox-restore-nixos-config" ''\n(.*?)\n\s*''\);`).FindSubmatch(data)
	if match == nil {
		t.Fatal("restore service script not found in the template")
	}
	script := strings.NewReplacer(
		"/home/emaland/.config/devbox", b.saved,
		"/etc/nixos", b.config,
		"${pkgs.bash}/bin/sh", "sh",
		"${./devbox/install_set.sh}", realInstallScript(),
	).Replace(string(match[1]))
	if strings.Contains(script, "${") && regexp.MustCompile(`\$\{(pkgs|\./)`).MatchString(script) {
		t.Fatal("unhandled Nix interpolation left in the restore script")
	}
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("restore service failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestRestoreServiceOldPathNeverWritesThroughTheLink(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "set in use\n", "1", nil)
	// The saved copy is still the old single file: save never reached /home.
	if err := os.WriteFile(filepath.Join(b.saved, "configuration.nix"), []byte("old single file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRestoreService(t, b)
	if got := read(t, filepath.Join(b.config, "sets", "20260101T000000Z", "configuration.nix")); got != "set in use\n" {
		t.Errorf("the old single file was written into the set in use: %q", got)
	}
}

func TestRestoreServiceRestoresTheSavedSet(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "saved set\n", "1", []byte("operator\n"))
	b.mustSave()
	b.fresh()
	runRestoreService(t, b)
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "saved set\n" {
		t.Errorf("in use after the restore service: %q", got)
	}
}

func TestRestoreServiceOldSingleFileStillRestores(t *testing.T) {
	b := newBox(t)
	if err := os.WriteFile(filepath.Join(b.saved, "configuration.nix"), []byte("old single file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRestoreService(t, b)
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "old single file\n" {
		t.Errorf("configuration.nix = %q", got)
	}
}

// The remote steps of a push, in order. Only the ordering and guards are
// checked here; the script itself is exercised by the tests above.
func TestRemoteApplyScript(t *testing.T) {
	script := remoteApplyScript("/tmp/devbox-push.abc123", "20260101T000000Z")
	at := func(needle string) int {
		t.Helper()
		i := strings.Index(script, needle)
		if i < 0 {
			t.Fatalf("apply script lacks %q:\n%s", needle, script)
		}
		return i
	}
	mount := at("mount /dev/disk/by-label/home-data /home")
	state := at("DEVBOX_SAVED_STATE=")
	install := at(" install /tmp/devbox-push.abc123/")
	rebuild := at("nixos-rebuild switch")
	save := at(" save " + remoteConfigDir + " " + remoteSavedDir)
	if !(mount < state && state < install && install < rebuild) {
		t.Error("order must be: mount /home, work out the saved state, install, then rebuild")
	}
	if !(install < save) {
		t.Error("the saved copy must be written only after the set is installed")
	}
	if !strings.Contains(script, "runuser -u emaland") {
		t.Error("the saved copy must be written as its owner, not by root")
	}
	if !strings.Contains(script, "mountpoint -q /home") {
		t.Error("the saved copy must be written only when /home is mounted")
	}
	if !strings.Contains(script, "rm -rf /tmp/devbox-push.abc123") {
		t.Error("the upload directory must be removed")
	}
	if strings.Contains(script, "'") {
		t.Error("the apply script is single-quoted by the caller and must contain no single quote")
	}
}

func TestRestoreNeverGoesBackToAnOlderSet(t *testing.T) {
	older := func(t *testing.T, newerStamp, newerLayout string) box {
		b := newBox(t)
		b.mustPush("20260101T000000Z", "saved, older\n", "1", nil)
		b.mustSave()
		// A newer push whose save step did not happen (/home not mounted, or
		// the save failed).
		b.mustPush(newerStamp, "in use, newer\n", newerLayout, nil)
		return b
	}
	for _, tc := range []struct{ name, stamp, layout string }{
		{"older layout", "20260110T000000Z", "2"},
		{"older stamp, same layout", "20260110T000000Z", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := older(t, tc.stamp, tc.layout)
			out, status := b.run("restore", b.config, b.saved)
			if status == 0 {
				t.Errorf("restore went back to an older set:\n%s", out)
			}
			if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "in use, newer\n" {
				t.Errorf("in use after restore: %q", got)
			}
		})
	}
}

func TestRestoreKeepsTheReplacedSetUnderItsName(t *testing.T) {
	b := newBox(t)
	b.mustPush("20260101T000000Z", "saved\n", "1", nil)
	b.mustSave()
	inUse := filepath.Join(b.config, "sets", "20260101T000000Z", "configuration.nix")
	if err := os.WriteFile(inUse, []byte("hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, status := b.run("restore", b.config, b.saved)
	if status != 0 {
		t.Fatalf("exit %d:\n%s", status, out)
	}
	target, err := os.Readlink(filepath.Join(b.config, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if target == "sets/20260101T000000Z" {
		t.Error("the restored set took the name of the set it replaced")
	}
	if got := read(t, inUse); got != "hand edit\n" {
		t.Errorf("the replaced set was changed in place: %q", got)
	}
	if got := read(t, filepath.Join(b.config, "configuration.nix")); got != "saved\n" {
		t.Errorf("in use: %q", got)
	}
	if !strings.Contains(out, "restored config set "+strings.TrimPrefix(target, "sets/")) {
		t.Errorf("the log does not name the set now in use (%s):\n%s", target, out)
	}
}

func TestRestoreFailsWhenTheSavedSetCannotBeReadCompletely(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read the file anyway")
	}
	b := newBox(t)
	b.mustPush("20260101T000000Z", "in use\n", "1", nil)
	b.mustSave()
	b.fresh()
	unreadable := filepath.Join(b.saved, "current", "devbox", "modules", "example.nix")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	out, status := b.run("restore", b.config, b.saved)
	if status == 0 {
		t.Errorf("a set that could not be copied completely was restored:\n%s", out)
	}
	if exists(filepath.Join(b.config, "current")) {
		t.Error("a set was switched to")
	}
}

func TestInstallRejectsAStampStartingWithADash(t *testing.T) {
	b := newBox(t)
	if out, status := b.push("-x", "x\n", "1", nil); status == 0 {
		t.Errorf("accepted:\n%s", out)
	}
}
