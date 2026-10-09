package credentialexec

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (boundary *Boundary) RefreshInputs(ctx context.Context, binding Binding, inputs map[string][]byte) error {
	if boundary == nil || ctx.Err() != nil || binding.Validate() != nil || ValidateInputs(inputs) != nil || len(inputs) == 0 || !boundary.requestMutex.TryLock() {
		return ErrUnavailable
	}
	defer boundary.requestMutex.Unlock()
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if boundary.closed || boundary.requestActive || binding != boundary.spec.Binding || boundary.stopErrorLocked() != nil || len(inputs) != len(boundary.spec.Inputs) {
		return ErrUnavailable
	}
	next := make(map[string][]byte, len(inputs))
	for name, contents := range inputs {
		if _, exists := boundary.spec.Inputs[name]; !exists {
			return ErrUnavailable
		}
		next[name] = bytes.Clone(contents)
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
	if accessInputsAt(ctx, root, boundary.spec.Inputs, false) != nil {
		return ErrUnavailable
	}
	if maps.EqualFunc(boundary.spec.Inputs, next, bytes.Equal) {
		return nil
	}
	path, err := os.MkdirTemp(boundary.state, "input-refresh-")
	if err != nil {
		return ErrUnavailable
	}
	defer os.RemoveAll(path)
	staging, err := openInputDirectory(int(state.Fd()), filepath.Base(path), false)
	if err != nil {
		return ErrUnavailable
	}
	defer staging.Close()
	if accessInputsAt(ctx, staging, next, true) != nil || staging.Sync() != nil || accessInputsAt(ctx, root, boundary.spec.Inputs, false) != nil {
		return ErrUnavailable
	}
	if ctx.Err() != nil || unix.Renameat2(int(staging.Fd()), "multica-input", int(root.Fd()), "multica-input", unix.RENAME_EXCHANGE) != nil {
		return ErrUnavailable
	}
	boundary.spec.Inputs = next
	return nil
}
