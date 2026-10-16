package credentialexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialInputsRefresh(test *testing.T) {
	initial := map[string][]byte{"multica-input/prompt.md": []byte("old prompt"), "multica-input/instructions.md": []byte("old instructions")}
	next := map[string][]byte{"multica-input/prompt.md": []byte("authorized prompt"), "multica-input/instructions.md": []byte("authorized instructions")}
	boundary := ownedInputMountBoundary(test, initial)
	boundary.spec.Binding = Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
	for _, name := range []string{"gateway.json", "binding.json", "home/native-session", "workdir/mutable-work"} {
		if err := os.WriteFile(filepath.Join(boundary.state, name), []byte("retained state"), 0600); err != nil {
			test.Fatal(err)
		}
	}
	for attempt := range 2 {
		if err := boundary.RefreshInputs(context.Background(), boundary.spec.Binding, next); err != nil {
			test.Fatalf("refresh %d refused: %v", attempt, err)
		}
		if err := boundary.VerifyInputs(context.Background()); err != nil {
			test.Fatal("refreshed manifest does not verify", err)
		}
		for name, expected := range next {
			contents, err := os.ReadFile(filepath.Join(boundary.WorkDir(), name))
			if err != nil || string(contents) != string(expected) {
				test.Fatal("authorized refresh did not replace input", name, err)
			}
		}
	}
	next["multica-input/prompt.md"][0] = 'X'
	if err := boundary.VerifyInputs(context.Background()); err != nil {
		test.Fatal("caller retained a mutable manifest alias", err)
	}
	for _, name := range []string{"gateway.json", "binding.json", "home/native-session", "workdir/mutable-work"} {
		contents, err := os.ReadFile(filepath.Join(boundary.state, name))
		if err != nil || string(contents) != "retained state" {
			test.Fatal("refresh altered identity, credentials or native state", name, err)
		}
	}
	entries, err := os.ReadDir(boundary.state)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "input-refresh-") {
			test.Fatal("refresh left owned staging state")
		}
	}
}

func TestCredentialInputsRefreshRefuses(test *testing.T) {
	for _, refusal := range []string{"binding", "cancelled", "closed", "stopped", "persisted-stop", "in-flight", "active-usage", "changed", "missing", "extra", "public", "linked", "new-path", "empty", "oversize"} {
		test.Run(refusal, func(test *testing.T) {
			initial := map[string][]byte{"multica-input/prompt.md": []byte("old prompt")}
			boundary := ownedInputMountBoundary(test, initial)
			boundary.spec.Binding = Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
			binding := boundary.spec.Binding
			next := map[string][]byte{"multica-input/prompt.md": []byte("authorized prompt")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(boundary.WorkDir(), "multica-input", "prompt.md")
			var err error
			switch refusal {
			case "binding":
				binding.OwnerID = "00000000-0000-4000-8000-000000000009"
			case "cancelled":
				cancel()
			case "closed":
				boundary.closed = true
			case "stopped":
				boundary.stopErr = ErrQuotaExhausted
			case "persisted-stop":
				err = os.WriteFile(filepath.Join(boundary.state, "gateway-stop"), []byte("unknown\n"), 0600)
			case "in-flight":
				boundary.requestMutex.Lock()
				defer boundary.requestMutex.Unlock()
			case "active-usage":
				boundary.requestActive = true
			case "changed":
				err = os.WriteFile(path, []byte("tampered"), 0600)
			case "missing":
				err = os.Remove(path)
			case "extra":
				err = os.WriteFile(filepath.Join(boundary.WorkDir(), "multica-input", "extra"), []byte("tampered"), 0600)
			case "public":
				err = os.Chmod(path, 0644)
			case "linked":
				err = os.Link(path, filepath.Join(boundary.WorkDir(), "linked"))
			case "new-path":
				next["multica-input/new.md"] = []byte("new input")
			case "empty":
				next = nil
			case "oversize":
				next["multica-input/prompt.md"] = make([]byte, (1<<20)+1)
			}
			if err != nil {
				test.Fatal(err)
			}
			if err := boundary.RefreshInputs(ctx, binding, next); !errors.Is(err, ErrUnavailable) {
				test.Fatal("unsafe refresh did not refuse", err)
			}
			contents, err := os.ReadFile(path)
			if refusal != "changed" && refusal != "missing" && (err != nil || string(contents) != "old prompt") {
				test.Fatal("refused refresh changed existing input", err)
			}
		})
	}
}
