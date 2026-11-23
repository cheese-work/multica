package oomwatch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// HeartbeatTag is the logger tag of the heartbeat nonce.
const HeartbeatTag = "multica-containment-hb"

var (
	bootHex = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uidNum  = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// NormalizeBootID turns /proc/sys/kernel/random/boot_id (with dashes) into the
// 32-hex form journald stores in _BOOT_ID.
func NormalizeBootID(s string) (string, error) {
	n := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
	if !bootHex.MatchString(n) {
		return "", fmt.Errorf("boot id %q is not 128 bits of hex", s)
	}
	return n, nil
}

// JournalctlArgs returns the argument list of the production follower (without
// the program name): the current boot's kernel messages, or the heartbeat
// lines this uid wrote, after the cursor. It refuses an empty cursor, a boot
// that is not 32 hex and a uid that is not numeric, so a value can never add a
// match of its own.
func JournalctlArgs(boot, afterCursor, uid string) ([]string, error) {
	switch {
	case afterCursor == "":
		return nil, errors.New("empty cursor")
	case !bootHex.MatchString(boot):
		return nil, fmt.Errorf("boot %q is not 32 lowercase hex", boot)
	case !uidNum.MatchString(uid):
		return nil, fmt.Errorf("uid %q is not numeric", uid)
	}
	return []string{
		"--follow", "--output=json", "--after-cursor=" + afterCursor,
		"_BOOT_ID=" + boot, "_TRANSPORT=kernel",
		"+",
		"_BOOT_ID=" + boot, "SYSLOG_IDENTIFIER=" + HeartbeatTag, "_UID=" + uid,
	}, nil
}
