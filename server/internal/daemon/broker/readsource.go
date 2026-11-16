package broker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ReadSource is the host-side read credential of one elevated run's read proxy
// (CHE-2512). An elevated run is refused a broker read token, so the proxy
// holds this one in daemon memory: it never reaches the sandbox. The token is
// read-only, names one repository, is reused until near expiry and is revoked
// by Close.
type ReadSource struct {
	tokens *Tokens
	repo   int64

	mu     sync.Mutex
	g      grant
	have   bool
	closed bool
}

// NewReadSource returns a source of read-only tokens for one repository.
func (t *Tokens) NewReadSource(repoID int64) *ReadSource {
	return &ReadSource{tokens: t, repo: repoID}
}

// Token returns a live read token, replacing it within ReadMargin of expiry.
func (s *ReadSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("read source closed")
	}
	if s.have && time.Until(s.g.expires) > or(s.tokens.ReadMargin, 10*time.Minute) {
		return s.g.token, nil
	}
	g, err := s.tokens.mint(ctx, s.repo, readPerms)
	if err != nil {
		return "", err
	}
	old, had := s.g, s.have
	s.g, s.have = g, true
	if had {
		_ = s.tokens.drop(ctx, old) // best effort: Close revokes the current token
	}
	return g.token, nil
}

// Close revokes the token. Later Token calls fail. Safe on repeat.
func (s *ReadSource) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if !s.have {
		return nil
	}
	return s.tokens.drop(ctx, s.g)
}
