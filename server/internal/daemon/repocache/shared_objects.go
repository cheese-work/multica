package repocache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Shared-objects checkouts (CHE-2216 slice A2c). The restricted sandbox mounts
// only the task workdir read-write and the cache object directory read-only,
// so a checkout must not depend on the cache's refs, config or worktree
// metadata. Its .git is a standalone directory with its own HEAD, refs and
// index; objects the task did not write are borrowed from the cache through
// objects/info/alternates.
//
// Borrowed objects only stay valid while the cache keeps them. Each task pins
// its base (and any carried branch tip) with a keep-ref in the cache, and
// records the task key in the checkout so cache maintenance can tell which
// pins still have a live borrower.

const (
	keepRefPrefix = "refs/multica/tasks/"
	// keepRefRecord lists, one per line, the task keys this checkout pinned.
	keepRefRecord = "multica-keep-refs"
)

// ObjectsDir is the cache object directory a shared-objects checkout borrows.
// A sandbox binds it read-only at this same absolute path.
func ObjectsDir(barePath string) string { return filepath.Join(barePath, "objects") }

func keepRefName(taskKey, kind string) string { return keepRefPrefix + taskKey + "/" + kind }

func hasAlternates(checkoutPath string) bool {
	info, err := os.Stat(filepath.Join(checkoutPath, ".git", "objects", "info", "alternates"))
	return err == nil && info.Mode().IsRegular()
}

// pinKeepRef points a task keep-ref in the cache at commit. The caller holds
// the repo lock.
func pinKeepRef(ctx context.Context, barePath, taskID, kind, commit string) error {
	key := taskKey(taskID)
	if key == "" {
		return fmt.Errorf("pin %s keep-ref: empty task id", kind)
	}
	if out, err := runGitCombinedOutputContext(ctx, "-C", barePath, "update-ref", keepRefName(key, kind), commit); err != nil {
		return fmt.Errorf("pin %s keep-ref: %s: %w", kind, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// recordKeepRef notes in the checkout that taskID's keep-refs have a borrower.
func recordKeepRef(checkoutPath, taskID string) error {
	key := taskKey(taskID)
	keys := CheckoutKeepRefTaskKeys(checkoutPath)
	if slices.Contains(keys, key) {
		return nil
	}
	keys = append(keys, key)
	slices.Sort(keys)
	path := filepath.Join(checkoutPath, ".git", keepRefRecord)
	return os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0o644)
}

// CheckoutKeepRefTaskKeys returns the task keys a checkout pinned in its cache.
func CheckoutKeepRefTaskKeys(checkoutPath string) []string {
	data, err := os.ReadFile(filepath.Join(checkoutPath, ".git", keepRefRecord))
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Fields(string(data)) {
		keys = append(keys, line)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// keepRefs lists the cache's keep-ref names.
func keepRefs(ctx context.Context, barePath string) ([]string, error) {
	out, err := runGitOutputContext(ctx, "-C", barePath, "for-each-ref", "--format=%(refname)", keepRefPrefix)
	if err != nil {
		return nil, fmt.Errorf("list keep-refs: %w", err)
	}
	return strings.Fields(string(out)), nil
}

func keepRefKey(ref string) string {
	key, _, _ := strings.Cut(strings.TrimPrefix(ref, keepRefPrefix), "/")
	return key
}

// KeepRefTaskKeys returns the sorted task keys that have a keep-ref in the cache.
func KeepRefTaskKeys(ctx context.Context, barePath string) ([]string, error) {
	refs, err := keepRefs(ctx, barePath)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, ref := range refs {
		keys = append(keys, keepRefKey(ref))
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// PruneKeepRefs deletes every keep-ref whose task key is not in live and
// reports how many it released and how many remain. The caller holds the
// repo lock.
func PruneKeepRefs(ctx context.Context, barePath string, live map[string]struct{}) (released, remaining int, err error) {
	refs, err := keepRefs(ctx, barePath)
	if err != nil {
		return 0, 0, err
	}
	for _, ref := range refs {
		if _, ok := live[keepRefKey(ref)]; ok {
			remaining++
			continue
		}
		if out, err := runGitCombinedOutputContext(ctx, "-C", barePath, "update-ref", "-d", ref); err != nil {
			return released, remaining + 1, fmt.Errorf("release %s: %s: %w", ref, strings.TrimSpace(string(out)), err)
		}
		released++
	}
	return released, remaining, nil
}
