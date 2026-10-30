package handler

import (
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	leaderSessionMaxTokensEnv     = "MULTICA_LEADER_SESSION_MAX_TOKENS"
	leaderSessionMaxTokensDefault = 80_000
)

// leaderSessionMaxTokens is the prompt size at which a squad leader's issue
// session stops being resumed. A leader resumes one session for the life of the
// issue, so every turn re-reads the whole history. An unset or invalid value
// uses the default.
func leaderSessionMaxTokens() int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(leaderSessionMaxTokensEnv)), 10, 64); err == nil && n > 0 {
		return n
	}
	return leaderSessionMaxTokensDefault
}

func leaderSessionTooLarge(isLeader bool, contextTokens pgtype.Int8) bool {
	return isLeader && contextTokens.Valid && contextTokens.Int64 >= leaderSessionMaxTokens()
}
