package credentialexec

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCredentialInputsVerifyWithoutRepair(test *testing.T) {
	for _, kind := range []string{"unchanged", "changed", "missing", "missing-directory", "symlink", "hardlink", "public", "public-directory", "fifo"} {
		test.Run(kind, func(test *testing.T) {
			inputs := map[string][]byte{"multica-input/prompt.md": []byte("authorized prompt")}
			boundary := &Boundary{state: test.TempDir(), spec: Spec{Inputs: inputs}}
			if err := os.Mkdir(boundary.WorkDir(), 0700); err != nil {
				test.Fatal(err)
			}
			if err := boundary.stageInputs(context.Background(), inputs); err != nil {
				test.Fatal(err)
			}
			path := filepath.Join(boundary.WorkDir(), "multica-input", "prompt.md")
			sentinel := filepath.Join(test.TempDir(), "credential")
			if err := os.WriteFile(sentinel, []byte("owned unrelated credential"), 0600); err != nil {
				test.Fatal(err)
			}
			var err error
			switch kind {
			case "changed":
				err = os.WriteFile(path, []byte("untrusted prompt"), 0600)
			case "missing", "missing-directory", "symlink", "hardlink", "fifo":
				err = os.Remove(path)
				if err == nil {
					switch kind {
					case "missing-directory":
						err = os.Remove(filepath.Dir(path))
					case "symlink":
						err = os.Symlink(sentinel, path)
					case "hardlink":
						err = os.Link(sentinel, path)
					case "fifo":
						err = unix.Mkfifo(path, 0600)
					}
				}
			case "public":
				err = os.Chmod(path, 0644)
			case "public-directory":
				err = os.Chmod(filepath.Dir(path), 0755)
			}
			if err != nil {
				test.Fatal(err)
			}
			err = boundary.VerifyInputs(context.Background())
			if kind == "unchanged" {
				if err != nil {
					test.Fatal("valid staged input cannot be verified", err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				test.Fatal("changed or unsafe staged input admitted", err)
			}
			if kind == "missing" || kind == "missing-directory" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					test.Fatal("verification recreated missing input", err)
				}
			}
			if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "owned unrelated credential" {
				test.Fatal("verification changed unrelated input", err)
			}
		})
	}
}

func TestCredentialInputsVerifyUnavailable(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, boundary := range []*Boundary{nil, {closed: true}, {stopErr: ErrOutcomeUnknown}} {
		if err := boundary.VerifyInputs(context.Background()); !errors.Is(err, ErrUnavailable) {
			test.Fatal("unavailable input verification admitted", err)
		}
	}
	boundary := &Boundary{state: test.TempDir()}
	if err := boundary.VerifyInputs(ctx); !errors.Is(err, ErrUnavailable) {
		test.Fatal("canceled input verification admitted", err)
	}
}

func TestCredentialInputsRetainState(test *testing.T) {
	boundary := &Boundary{state: test.TempDir()}
	if err := os.Mkdir(boundary.WorkDir(), 0700); err != nil {
		test.Fatal(err)
	}
	inputs := map[string][]byte{"multica-input/prompt.md": []byte("authorized prompt"), "multica-input/skills/owned/SKILL.md": []byte("authorized skill")}
	for attempt := 0; attempt < 2; attempt++ {
		if err := boundary.stageInputs(context.Background(), inputs); err != nil {
			test.Fatal(err)
		}
		for name, expected := range inputs {
			path := filepath.Join(boundary.WorkDir(), name)
			contents, err := os.ReadFile(path)
			info, statErr := os.Stat(path)
			if err != nil || string(contents) != string(expected) || statErr != nil || info.Mode().Perm() != 0600 {
				test.Fatalf("input was not staged exactly and privately: %s", name)
			}
		}
		if err := os.WriteFile(filepath.Join(boundary.WorkDir(), "native-state"), []byte("retained"), 0600); err != nil {
			test.Fatal(err)
		}
	}
	inputs["multica-input/prompt.md"] = []byte("changed input")
	if err := boundary.stageInputs(context.Background(), inputs); !errors.Is(err, ErrUnavailable) {
		test.Fatal("changed same-task input was overwritten")
	}
	if contents, err := os.ReadFile(filepath.Join(boundary.WorkDir(), "native-state")); err != nil || string(contents) != "retained" {
		test.Fatal("staging reset native state")
	}
}

func TestCredentialInputsRefuseUnsafeDestination(test *testing.T) {
	for _, kind := range []string{"symlink-root", "symlink-directory", "symlink-file", "hardlink", "fifo", "socket", "directory", "public-directory", "public", "changed"} {
		test.Run(kind, func(test *testing.T) {
			boundary := &Boundary{state: test.TempDir()}
			if err := os.Mkdir(boundary.WorkDir(), 0700); err != nil {
				test.Fatal(err)
			}
			outside := test.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("owned unlimited credential"), 0600); err != nil {
				test.Fatal(err)
			}
			name := "multica-input/skills/owned/proof.md"
			path := filepath.Join(boundary.WorkDir(), name)
			if kind == "symlink-root" {
				if err := os.Symlink(outside, filepath.Join(boundary.WorkDir(), "multica-input")); err != nil {
					test.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					test.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink-directory":
					err = os.Remove(filepath.Dir(path))
					if err == nil {
						err = os.Symlink(outside, filepath.Dir(path))
					}
				case "symlink-file":
					err = os.Symlink(sentinel, path)
				case "hardlink":
					err = os.Link(sentinel, path)
				case "fifo":
					err = unix.Mkfifo(path, 0600)
				case "socket":
					var directory *os.File
					directory, err = os.Open(filepath.Dir(path))
					if err != nil {
						break
					}
					defer directory.Close()
					var listener net.Listener
					listener, err = net.Listen("unix", "/proc/self/fd/"+strconv.FormatUint(uint64(directory.Fd()), 10)+"/"+filepath.Base(path))
					if err == nil {
						defer listener.Close()
					}
				case "directory":
					err = os.Mkdir(path, 0700)
				case "public-directory":
					err = os.Chmod(filepath.Dir(path), 0755)
				case "public":
					err = os.WriteFile(path, []byte("authorized"), 0644)
				case "changed":
					err = os.WriteFile(path, []byte("forged"), 0600)
				}
				if err != nil {
					test.Fatal(err)
				}
			}
			var stageErr error
			if kind == "symlink-file" && os.Getenv("MULTICA_FIXTURE_ASSERT_ORDINARY_INPUT_SAFE") == "1" {
				stageErr = os.WriteFile(path, []byte("authorized"), 0600)
				test.Log("NEGATIVE CONTROL: ordinary copy follows an owned unlimited-credential symlink")
			} else {
				stageErr = boundary.stageInputs(context.Background(), map[string][]byte{name: []byte("authorized")})
			}
			if !errors.Is(stageErr, ErrUnavailable) {
				test.Fatalf("unsafe input destination admitted: %v", stageErr)
			}
			if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "owned unlimited credential" {
				test.Fatal("staging read/overwrote an unrelated credential")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
				test.Fatal("staging escaped the task workdir")
			}
		})
	}
}

func TestCredentialInputsRefuseInvalidManifest(test *testing.T) {
	for _, name := range []string{"", "/absolute", "../escape", "multica-input/../escape", "multica-input//prompt.md", "multica-input/prompt.md/", "multica-input\\prompt.md", "multica-input/\x00", "auth.json", "multica-input/./prompt.md"} {
		test.Run(name, func(test *testing.T) {
			if err := ValidateInputs(map[string][]byte{name: []byte("authorized")}); !errors.Is(err, ErrUnavailable) {
				test.Fatal("invalid input path admitted")
			}
		})
	}
	if err := ValidateInputs(map[string][]byte{"multica-input/prompt.md": make([]byte, 1<<20+1)}); !errors.Is(err, ErrUnavailable) {
		test.Fatal("oversized input admitted")
	}
	if err := ValidateInputs(map[string][]byte{"multica-input/file": nil, "multica-input/file/child": nil}); !errors.Is(err, ErrUnavailable) {
		test.Fatal("file/directory collision admitted")
	}
	inputs := make(map[string][]byte)
	for index := 0; index < 129; index++ {
		inputs["multica-input/"+strconv.Itoa(index)] = nil
	}
	if err := ValidateInputs(inputs); !errors.Is(err, ErrUnavailable) {
		test.Fatal("unbounded file count admitted")
	}
	inputs = make(map[string][]byte)
	for index := 0; index < 9; index++ {
		inputs["multica-input/"+strconv.Itoa(index)] = make([]byte, 1<<20)
	}
	if err := ValidateInputs(inputs); !errors.Is(err, ErrUnavailable) {
		test.Fatal("unbounded total input size admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	boundary := &Boundary{state: test.TempDir()}
	if err := boundary.stageInputs(ctx, map[string][]byte{"multica-input/prompt.md": []byte("authorized")}); !errors.Is(err, ErrUnavailable) {
		test.Fatal("cancelled staging admitted")
	}
}
