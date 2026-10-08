package credentialexec

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type runtimeIdentity struct {
	Binding    Binding
	Provider   string
	Executable string
	Helper     string
	Digests    map[string]string
}

func Prepare(ctx context.Context, spec Spec) (*Boundary, error) {
	if err := ValidateInputs(spec.Inputs); err != nil {
		return nil, err
	}
	if err := spec.Binding.Validate(); err != nil {
		return nil, err
	}
	if runtime.GOARCH != "amd64" || spec.Provider != "claude" && spec.Provider != "codex" || !filepath.IsAbs(spec.Root) || !filepath.IsAbs(spec.Executable) || !filepath.IsAbs(spec.HelperExecutable) {
		return nil, fmt.Errorf("%w: unsupported provider or relative source path", ErrUnavailable)
	}
	if err := privateDirectory(spec.Root); err != nil {
		return nil, err
	}
	state := filepath.Join(spec.Root, spec.Binding.TaskID)
	if err := readGatewayStop(state); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(state); err == nil && len(entries) != 0 {
		info, markerErr := os.Lstat(filepath.Join(state, "binding.json"))
		if markerErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("%w: existing task state lacks an owned binding", ErrUnavailable)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: existing task state is unavailable", ErrUnavailable)
	}
	for _, directory := range []string{state, filepath.Join(state, "home"), filepath.Join(state, "workdir")} {
		if err := privateDirectory(directory); err != nil {
			return nil, err
		}
	}
	identity := runtimeIdentity{Binding: spec.Binding, Provider: spec.Provider, Executable: spec.Executable, Helper: spec.HelperExecutable, Digests: make(map[string]string)}
	assets := make(map[string][]byte)
	for _, path := range []string{spec.Executable, spec.HelperExecutable, "/bin/sh", "/bin/true"} {
		if err := collectRuntimeAsset(path, assets); err != nil {
			return nil, err
		}
	}
	for path, data := range assets {
		digest := sha256.Sum256(data)
		identity.Digests[path] = hex.EncodeToString(digest[:])
	}
	identityData, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	identityPath := filepath.Join(state, "binding.json")
	file, err := os.OpenFile(identityPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		existing, readErr := os.ReadFile(identityPath)
		if readErr != nil || string(existing) != string(identityData) {
			return nil, fmt.Errorf("%w: same-task source identity changed", ErrUnavailable)
		}
	} else if err != nil {
		return nil, fmt.Errorf("%w: record binding", ErrUnavailable)
	} else {
		_, writeErr := file.Write(identityData)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, fmt.Errorf("%w: persist binding", ErrUnavailable)
		}
	}
	runtimeRoot := filepath.Join(state, "runtime")
	if err := privateDirectory(runtimeRoot); err != nil {
		return nil, err
	}
	for path, data := range assets {
		destination := filepath.Join(runtimeRoot, path)
		if path == spec.Executable {
			destination = filepath.Join(runtimeRoot, "runtime", filepath.Base(spec.Executable))
		}
		if path == spec.HelperExecutable {
			destination = filepath.Join(runtimeRoot, "runtime", "multica-helper")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return nil, err
		}
		if existing, err := os.ReadFile(destination); err == nil {
			if string(existing) != string(data) {
				return nil, fmt.Errorf("%w: runtime snapshot changed", ErrUnavailable)
			}
		} else if os.IsNotExist(err) {
			if err := os.WriteFile(destination, data, 0500); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	for _, directory := range []string{state + "/home", state + "/workdir", "/proc", "/dev", "/tmp", "/run"} {
		if err := os.MkdirAll(filepath.Join(runtimeRoot, directory), 0700); err != nil {
			return nil, err
		}
	}
	socketPlaceholder := filepath.Join(runtimeRoot, "run", "gateway.sock")
	placeholder, err := os.OpenFile(socketPlaceholder, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := placeholder.Close(); err != nil {
		return nil, err
	}
	boundary := &Boundary{spec: spec, state: state, runtimeRoot: runtimeRoot, bwrap: "/usr/bin/bwrap", stopped: make(chan struct{})}
	home, err := os.Open(boundary.Home())
	if err != nil {
		return nil, err
	}
	defer home.Close()
	for _, directory := range []string{".codex", ".claude", ".config", ".cache", ".data"} {
		if err := unix.Mkdirat(int(home.Fd()), directory, 0700); err != nil && err != unix.EEXIST {
			return nil, err
		}
		descriptor, err := unix.Openat2(int(home.Fd()), directory, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
		if err != nil {
			return nil, fmt.Errorf("%w: unsafe native state directory", ErrUnavailable)
		}
		if err := unix.Close(descriptor); err != nil {
			return nil, err
		}
	}
	if err := boundary.Probe(ctx); err != nil {
		return nil, err
	}
	if err := boundary.stageInputs(ctx, spec.Inputs); err != nil {
		return nil, err
	}
	return boundary, nil
}

func privateDirectory(path string) error {
	if filepath.Clean(path) != path {
		return fmt.Errorf("%w: unclean state path", ErrUnavailable)
	}
	for ancestor := path; ancestor != "/"; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: state ancestor unavailable", ErrUnavailable)
		}
		if err == nil && !info.IsDir() {
			return fmt.Errorf("%w: state ancestor is not a directory", ErrUnavailable)
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("%w: private state directory", ErrUnavailable)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("%w: state is not a private directory", ErrUnavailable)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%w: state directory owner mismatch", ErrUnavailable)
	}
	return nil
}

func collectRuntimeAsset(path string, assets map[string][]byte) error {
	if _, exists := assets[path]; exists {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: resolve runtime asset", ErrUnavailable)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return fmt.Errorf("%w: open runtime asset", ErrUnavailable)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: nonregular runtime asset", ErrUnavailable)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("%w: read runtime asset", ErrUnavailable)
	}
	assets[path] = data
	image, err := elf.NewFile(file)
	if err != nil {
		return fmt.Errorf("%w: only native ELF assets are supported", ErrUnavailable)
	}
	for _, program := range image.Progs {
		if program.Type == elf.PT_INTERP {
			interpreter, err := io.ReadAll(program.Open())
			if err != nil {
				return err
			}
			loader := strings.TrimRight(string(interpreter), "\x00")
			if loader != "/lib64/ld-linux-x86-64.so.2" && loader != "/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2" && loader != "/lib/ld-musl-x86_64.so.1" {
				return fmt.Errorf("%w: unsupported ELF interpreter", ErrUnavailable)
			}
			if err := collectRuntimeAsset(loader, assets); err != nil {
				return err
			}
		}
	}
	libraries, err := image.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%w: runtime library manifest", ErrUnavailable)
	}
	for _, library := range libraries {
		if filepath.Base(library) != library {
			return fmt.Errorf("%w: unsafe native library path", ErrUnavailable)
		}
		found := false
		for _, directory := range []string{"/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/lib64", "/lib", "/usr/lib"} {
			candidate := filepath.Join(directory, library)
			if _, err := os.Stat(candidate); err == nil {
				if err := collectRuntimeAsset(candidate, assets); err != nil {
					return err
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: unsupported native library", ErrUnavailable)
		}
	}
	return nil
}

func (boundary *Boundary) Probe(ctx context.Context) error {
	info, err := os.Stat(boundary.bwrap)
	if err != nil {
		return fmt.Errorf("%w: bubblewrap missing", ErrUnavailable)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	systemDirectory, directoryErr := os.Stat("/usr/bin")
	if directoryErr != nil {
		return fmt.Errorf("%w: system executable directory unavailable", ErrUnavailable)
	}
	directoryStat, directoryOK := systemDirectory.Sys().(*syscall.Stat_t)
	if boundary.bwrap != "/usr/bin/bwrap" || !ok || !directoryOK || stat.Uid != directoryStat.Uid || stat.Uid == uint32(os.Getuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || systemDirectory.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%w: untrusted bubblewrap executable", ErrUnavailable)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, "/bin/true")
	command.Dir = boundary.WorkDir()
	cleanup, err := boundary.wrap(command, false)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: namespace/read-boundary control failed", ErrUnavailable)
	}
	return nil
}

func (boundary *Boundary) Wrap(command *exec.Cmd) (func(), error) {
	return boundary.wrap(command, true)
}

func (boundary *Boundary) wrap(command *exec.Cmd, withGateway bool) (func(), error) {
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if err := boundary.stopErrorLocked(); err != nil {
		return nil, err
	}
	if boundary.closed || command.Dir != boundary.WorkDir() || len(command.ExtraFiles) != 0 || withGateway && (boundary.server == nil || command.Path != boundary.spec.Executable) {
		return nil, fmt.Errorf("%w: unsupported launch, descriptor or missing gateway", ErrUnavailable)
	}
	stateFile, err := os.Open(boundary.state)
	if err != nil {
		return nil, fmt.Errorf("%w: state anchor unavailable", ErrUnavailable)
	}
	defer stateFile.Close()
	var files []*os.File
	cleanup := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	for _, directory := range []string{"home", "workdir", "runtime"} {
		descriptor, err := unix.Openat2(int(stateFile.Fd()), directory, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("%w: state mount validation failed", ErrUnavailable)
		}
		files = append(files, os.NewFile(uintptr(descriptor), directory))
	}
	arguments := []string{boundary.bwrap, "--unshare-all", "--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--ro-bind-fd", "5", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--bind-fd", "3", boundary.Home(), "--bind-fd", "4", boundary.WorkDir(), "--chdir", boundary.WorkDir()}
	if withGateway {
		if ValidateInputs(boundary.spec.Inputs) != nil {
			cleanup()
			return nil, ErrUnavailable
		}
		names := make([]string, 0, len(boundary.spec.Inputs))
		for name := range boundary.spec.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			file, err := accessInput(files[1], name, boundary.spec.Inputs[name], false)
			if err != nil {
				cleanup()
				return nil, ErrUnavailable
			}
			arguments = append(arguments, "--ro-bind-fd", strconv.Itoa(len(files)+3), filepath.Join(boundary.WorkDir(), name))
			files = append(files, file)
		}
	}
	environment := boundary.Environment()
	nativeArgs := command.Args
	if withGateway {
		nativeArgs = append([]string{"/" + filepath.Join("runtime", filepath.Base(boundary.spec.Executable))}, command.Args[1:]...)
	}
	if withGateway {
		arguments = append(arguments, "--ro-bind", boundary.socket, "/run/gateway.sock", "--", "/runtime/multica-helper", HelperArg)
		environment["MULTICA_CREDENTIAL_GATEWAY_SOCKET"] = "/run/gateway.sock"
	} else {
		arguments = append(arguments, "--")
	}
	arguments = append(arguments, nativeArgs...)
	command.Path, command.Args, command.ExtraFiles = boundary.bwrap, arguments, files
	command.Env = nil
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	sort.Strings(command.Env)
	return cleanup, nil
}
