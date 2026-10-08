package credentialexec

import (
	"bytes"
	"context"
	"io"
	"os"
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
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil || accessInput(root, name, inputs[name], create) != nil {
			return ErrUnavailable
		}
	}
	return nil
}

func openInputDirectory(parent int, name string, create bool) (*os.File, error) {
	if create {
		if err := unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
			return nil, ErrUnavailable
		}
	}
	descriptor, err := unix.Openat2(parent, name, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
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

func accessInput(root *os.File, name string, contents []byte, create bool) error {
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
			return ErrUnavailable
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
		return ErrUnavailable
	}
	file := os.NewFile(uintptr(descriptor), filename)
	defer file.Close()
	var info unix.Stat_t
	if unix.Fstat(descriptor, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Nlink != 1 || info.Uid != uint32(os.Getuid()) {
		return ErrUnavailable
	}
	if existing {
		if info.Size != int64(len(contents)) {
			return ErrUnavailable
		}
		stored, err := io.ReadAll(io.LimitReader(file, int64(len(contents))+1))
		if err != nil || !bytes.Equal(stored, contents) {
			return ErrUnavailable
		}
		return nil
	}
	if count, err := file.Write(contents); err != nil || count != len(contents) {
		return ErrUnavailable
	}
	if file.Sync() != nil || file.Close() != nil {
		return ErrUnavailable
	}
	return nil
}
