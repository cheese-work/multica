package credentialexec

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCredentialInputsMountVerifiedDescriptors(test *testing.T) {
	inputs := map[string][]byte{
		"multica-input/prompt.md":       []byte("authorized prompt"),
		"multica-input/instructions.md": []byte("authorized instructions"),
	}
	boundary := ownedInputMountBoundary(test, inputs)
	command := exec.Command(boundary.spec.Executable)
	command.Dir = boundary.WorkDir()
	cleanup, err := boundary.Wrap(command)
	if err != nil {
		test.Fatal(err)
	}
	defer cleanup()
	if len(command.ExtraFiles) != 6 {
		test.Fatalf("verified input descriptors missing: %d", len(command.ExtraFiles))
	}
	var mounts []string
	for index, argument := range command.Args {
		if argument == "--ro-bind-fd" && command.Args[index+1] != "5" {
			mounts = append(mounts, command.Args[index+1:index+3]...)
		}
	}
	expected := []string{"6", filepath.Join(boundary.WorkDir(), "multica-input"), "7", filepath.Join(boundary.WorkDir(), "multica-input", "instructions.md"), "8", filepath.Join(boundary.WorkDir(), "multica-input", "prompt.md")}
	if !reflect.DeepEqual(mounts, expected) {
		test.Fatalf("input mounts are not exact and deterministic: %v", mounts)
	}
	for index, name := range []string{"multica-input/instructions.md", "multica-input/prompt.md"} {
		path := filepath.Join(boundary.WorkDir(), name)
		if err := os.Rename(path, path+".old"); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement input"), 0600); err != nil {
			test.Fatal(err)
		}
		file := command.ExtraFiles[index+4]
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			test.Fatal(err)
		}
		contents, err := io.ReadAll(file)
		if err != nil || string(contents) != string(inputs[name]) {
			test.Fatal("input descriptor followed a replaced path", err)
		}
		if _, err := file.Write([]byte("untrusted")); err == nil {
			test.Fatal("input descriptor permits writing")
		}
	}
	cleanup()
	for _, file := range command.ExtraFiles {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			test.Fatal("launch cleanup leaked an owned descriptor", err)
		}
	}
}

func TestCredentialInputsMountNativeAliases(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			inputs := map[string][]byte{
				"multica-input/runtime.md":             []byte("authorized runtime brief"),
				"multica-input/project/resources.json": []byte(`{"resources":[]}`),
				"multica-input/skills/owned/SKILL.md":  []byte("authorized skill"),
			}
			boundary := ownedInputMountBoundary(test, inputs)
			boundary.spec.Provider = provider
			nativeRoot, briefName := ".agents", "AGENTS.md"
			if provider == "claude" {
				nativeRoot, briefName = ".claude", "CLAUDE.md"
			}
			command := exec.Command(boundary.spec.Executable)
			command.Dir = boundary.WorkDir()
			cleanup, err := boundary.Wrap(command)
			if err != nil {
				test.Fatal(err)
			}
			defer cleanup()
			mounts := make(map[string]*os.File)
			for index, argument := range command.Args {
				if argument == "--ro-bind-fd" {
					descriptor, err := strconv.Atoi(command.Args[index+1])
					if err != nil {
						test.Fatal(err)
					}
					mounts[command.Args[index+2]] = command.ExtraFiles[descriptor-3]
				}
			}
			if len(command.ExtraFiles) != 16 {
				test.Fatal("native alias descriptors missing", len(command.ExtraFiles))
			}
			for _, file := range command.ExtraFiles {
				flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					test.Fatal("native input descriptor can leak to unrelated children", err)
				}
			}
			for name, contents := range inputs {
				path := filepath.Join(boundary.WorkDir(), name)
				if err := os.Rename(path, path+".old"); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("replacement input"), 0600); err != nil {
					test.Fatal(err)
				}
				for _, root := range []string{"multica-input", nativeRoot, ".multica"} {
					alias := filepath.Join(boundary.WorkDir(), root, name[len("multica-input/"):])
					file := mounts[alias]
					if file == nil {
						test.Fatal("missing native input mount", alias)
					}
					if _, err := file.Seek(0, io.SeekStart); err != nil {
						test.Fatal(err)
					}
					observed, err := io.ReadAll(file)
					if err != nil || string(observed) != string(contents) {
						test.Fatal("native alias followed a replaced path", alias, err)
					}
					if _, err := file.Write([]byte("forged")); err == nil {
						test.Fatal("native input descriptor permits writing", alias)
					}
				}
			}
			brief := mounts[filepath.Join(boundary.WorkDir(), briefName)]
			if brief == nil {
				test.Fatal("native runtime brief missing")
			}
			if _, err := brief.Seek(0, io.SeekStart); err != nil {
				test.Fatal(err)
			}
			if contents, err := io.ReadAll(brief); err != nil || string(contents) != string(inputs["multica-input/runtime.md"]) {
				test.Fatal("native runtime brief descriptor followed replaced input", err)
			}
			cleanup()
			for _, file := range command.ExtraFiles {
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					test.Fatal("native alias cleanup leaked descriptor", err)
				}
			}
		})
	}
}

func TestCredentialInputsMountRefusesUnsafeNativeDestinations(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, kind := range []string{"symlink", "wrong-type"} {
			for _, name := range []string{".agents", ".multica", "AGENTS.md"} {
				directory := name != "AGENTS.md"
				if provider == "claude" && name == ".agents" {
					name = ".claude"
				}
				if provider == "claude" && name == "AGENTS.md" {
					name = "CLAUDE.md"
				}
				test.Run(provider+"/"+kind+"/"+name, func(test *testing.T) {
					boundary := ownedInputMountBoundary(test, map[string][]byte{"multica-input/runtime.md": []byte("authorized brief")})
					boundary.spec.Provider = provider
					path := filepath.Join(boundary.WorkDir(), name)
					var err error
					if kind == "symlink" {
						err = os.Symlink(boundary.Home(), path)
					} else if directory {
						err = os.WriteFile(path, []byte("untrusted alias"), 0600)
					} else {
						err = os.Mkdir(path, 0700)
					}
					if err != nil {
						test.Fatal(err)
					}
					command := exec.Command(boundary.spec.Executable)
					command.Dir = boundary.WorkDir()
					cleanup, err := boundary.Wrap(command)
					if cleanup != nil {
						cleanup()
					}
					if !errors.Is(err, ErrUnavailable) || len(command.ExtraFiles) != 0 {
						test.Fatal("unsafe native destination admitted", err)
					}
				})
			}
		}
	}
}

func TestCredentialInputsMountRefusesChangedSnapshot(test *testing.T) {
	for _, kind := range []string{"changed", "missing", "symlink", "hardlink", "fifo", "public", "public-directory", "invalid-manifest", "extra-file", "extra-directory", "extra-symlink"} {
		test.Run(kind, func(test *testing.T) {
			name := "multica-input/prompt.md"
			boundary := ownedInputMountBoundary(test, map[string][]byte{name: []byte("authorized prompt")})
			path := filepath.Join(boundary.WorkDir(), name)
			sentinel := filepath.Join(test.TempDir(), "unrelated-credential")
			if err := os.WriteFile(sentinel, []byte("owned unrelated credential"), 0600); err != nil {
				test.Fatal(err)
			}
			var err error
			switch kind {
			case "changed":
				err = os.WriteFile(path, []byte("forged input"), 0600)
			case "public":
				err = os.Chmod(path, 0644)
			case "public-directory":
				err = os.Chmod(filepath.Dir(path), 0755)
			case "invalid-manifest":
				boundary.spec.Inputs["../escape"] = []byte("invalid")
			case "extra-file":
				err = os.WriteFile(filepath.Join(filepath.Dir(path), "unexpected"), []byte("untrusted"), 0600)
			case "extra-directory":
				err = os.Mkdir(filepath.Join(filepath.Dir(path), "unexpected"), 0700)
			case "extra-symlink":
				err = os.Symlink(sentinel, filepath.Join(filepath.Dir(path), "unexpected"))
			default:
				err = os.Remove(path)
				if err == nil {
					switch kind {
					case "symlink":
						err = os.Symlink(sentinel, path)
					case "hardlink":
						err = os.Link(sentinel, path)
					case "fifo":
						err = unix.Mkfifo(path, 0600)
					}
				}
			}
			if err != nil {
				test.Fatal(err)
			}
			command := exec.Command(boundary.spec.Executable)
			command.Dir = boundary.WorkDir()
			cleanup, err := boundary.Wrap(command)
			if cleanup != nil {
				cleanup()
			}
			if !errors.Is(err, ErrUnavailable) || len(command.ExtraFiles) != 0 {
				test.Fatal("unsafe input admitted by direct native wrapping", err)
			}
			if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "owned unrelated credential" {
				test.Fatal("wrapping changed an unrelated credential", err)
			}
		})
	}
}

func TestCredentialInputsMountPreservesInputFreeAndProbe(test *testing.T) {
	for _, probe := range []bool{false, true} {
		test.Run(strconv.FormatBool(probe), func(test *testing.T) {
			var inputs map[string][]byte
			if probe {
				inputs = map[string][]byte{"multica-input/prompt.md": []byte("authorized prompt")}
			}
			boundary := ownedInputMountBoundary(test, inputs)
			command := exec.Command(boundary.spec.Executable)
			command.Dir = boundary.WorkDir()
			cleanup, err := boundary.wrap(command, !probe)
			if err != nil {
				test.Fatal(err)
			}
			defer cleanup()
			if len(command.ExtraFiles) != 3 {
				test.Fatal("input-free or probe launch acquired input descriptors")
			}
			for _, argument := range command.Args {
				if argument == filepath.Join(boundary.WorkDir(), "multica-input") {
					test.Fatal("input-free or probe launch acquired an input mount")
				}
			}
		})
	}
}

func ownedInputMountBoundary(test *testing.T, inputs map[string][]byte) *Boundary {
	test.Helper()
	boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "codex", Executable: "/owned-native", Inputs: inputs}, server: &http.Server{}, bwrap: "/usr/bin/bwrap"}
	for _, name := range []string{"home", "workdir", "runtime"} {
		if err := os.Mkdir(filepath.Join(boundary.state, name), 0700); err != nil {
			test.Fatal(err)
		}
	}
	if err := boundary.stageInputs(context.Background(), inputs); err != nil {
		test.Fatal(err)
	}
	return boundary
}
