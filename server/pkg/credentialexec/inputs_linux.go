package credentialexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func (boundary *Boundary) stageInputs(ctx context.Context, inputs map[string][]byte) error {
	return boundary.accessInputs(ctx, inputs, true)
}

func (boundary *Boundary) VerifyInputs(ctx context.Context) error {
	if boundary == nil {
		return ErrUnavailable
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if boundary.closed || boundary.stopErrorLocked() != nil {
		return ErrUnavailable
	}
	return boundary.accessInputs(ctx, boundary.spec.Inputs, false)
}

func (boundary *Boundary) accessInputs(ctx context.Context, inputs map[string][]byte, create bool) error {
	if ctx.Err() != nil || ValidateInputs(inputs) != nil {
		return ErrUnavailable
	}
	if len(inputs) == 0 {
		return nil
	}
	state, err := os.Open(boundary.state)
	if err != nil {
		return ErrUnavailable
	}
	defer state.Close()
	root, err := openInputDirectory(int(state.Fd()), "workdir", false)
	if err != nil {
		return ErrUnavailable
	}
	defer root.Close()
	return accessInputsAt(ctx, root, inputs, create)
}

func accessInputsAt(ctx context.Context, root *os.File, inputs map[string][]byte, create bool) error {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return ErrUnavailable
		}
		file, err := accessInput(root, name, inputs[name], create)
		if err != nil {
			return ErrUnavailable
		}
		if file.Close() != nil {
			return ErrUnavailable
		}
	}
	inputRoot, err := openInputDirectory(int(root.Fd()), "multica-input", false)
	if err != nil {
		return ErrUnavailable
	}
	defer inputRoot.Close()
	return verifyInputTree(inputRoot, inputs)
}

func verifyInputTree(root *os.File, inputs map[string][]byte) error {
	expected := make(map[string]bool)
	for name := range inputs {
		name = strings.TrimPrefix(name, "multica-input/")
		expected[name] = false
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			expected[parent] = true
		}
	}
	seen := 0
	var verify func(*os.File, string) error
	verify = func(directory *os.File, prefix string) error {
		entries, err := directory.ReadDir(len(expected) + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return ErrUnavailable
		}
		for _, entry := range entries {
			name := filepath.Join(prefix, entry.Name())
			isDirectory, exists := expected[name]
			if !exists || entry.IsDir() != isDirectory {
				return ErrUnavailable
			}
			seen++
			if isDirectory {
				child, err := openInputDirectory(int(directory.Fd()), entry.Name(), false)
				if err != nil {
					return ErrUnavailable
				}
				err = verify(child, name)
				closeErr := child.Close()
				if err != nil || closeErr != nil {
					return ErrUnavailable
				}
			}
		}
		return nil
	}
	if verify(root, "") != nil || seen != len(expected) {
		return ErrUnavailable
	}
	return nil
}

func openInputDirectory(parent int, name string, create bool) (*os.File, error) {
	if create {
		if err := unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
			return nil, ErrUnavailable
		}
	}
	descriptor, err := unix.Openat2(parent, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(descriptor), name)
	var info unix.Stat_t
	if unix.Fstat(descriptor, &info) != nil || info.Uid != uint32(os.Getuid()) || info.Mode&07777 != 0700 {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	return file, nil
}

func accessInput(root *os.File, name string, contents []byte, create bool) (*os.File, error) {
	parent := root
	var directories []*os.File
	defer func() {
		for _, directory := range directories {
			_ = directory.Close()
		}
	}()
	parts := strings.Split(name, "/")
	for _, component := range parts[:len(parts)-1] {
		directory, err := openInputDirectory(int(parent.Fd()), component, create)
		if err != nil {
			return nil, ErrUnavailable
		}
		directories = append(directories, directory)
		parent = directory
	}
	filename := parts[len(parts)-1]
	options := &unix.OpenHow{Flags: unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NONBLOCK, Mode: 0600, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS}
	if !create {
		options.Flags, options.Mode = unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0
	}
	descriptor, err := unix.Openat2(int(parent.Fd()), filename, options)
	existing := !create || err == unix.EEXIST
	if err == unix.EEXIST {
		options.Flags, options.Mode = unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0
		descriptor, err = unix.Openat2(int(parent.Fd()), filename, options)
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(descriptor), filename)
	retained := false
	defer func() {
		if !retained {
			_ = file.Close()
		}
	}()
	var info unix.Stat_t
	if unix.Fstat(descriptor, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Nlink != 1 || info.Uid != uint32(os.Getuid()) {
		return nil, ErrUnavailable
	}
	if existing {
		if info.Size != int64(len(contents)) {
			return nil, ErrUnavailable
		}
		stored, err := io.ReadAll(io.LimitReader(file, int64(len(contents))+1))
		if err != nil || !bytes.Equal(stored, contents) {
			return nil, ErrUnavailable
		}
		retained = true
		return file, nil
	}
	if count, err := file.Write(contents); err != nil || count != len(contents) {
		return nil, ErrUnavailable
	}
	if file.Sync() != nil {
		return nil, ErrUnavailable
	}
	retained = true
	return file, nil
}
