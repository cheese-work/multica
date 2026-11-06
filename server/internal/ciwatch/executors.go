package ciwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RegisterStormExecutors wires issue_create, issue_comment and inbox_notify.
// A nil destination keeps its kind Shadow-only: the action ends failed with
// non_transactional_destination, and no lookup-then-create fallback exists.
func (l *Ledger) RegisterStormExecutors(d Destinations) {
	l.RegisterLocal(ActionIssueCreate, l.issueCreate(d))
	l.RegisterLocal(ActionIssueComment, l.issueComment(d))
	l.RegisterLocal(ActionInboxNotify, l.inboxNotify(d))
}

func nonTransactional(what string) error {
	return &PermanentError{Reason: ReasonNonTransactional, Err: errors.New("no transactional " + what + " destination")}
}

type watchConfig struct {
	FixOwnerKind, FixOwnerID, NotifyMemberID string
	AuthorizedByMemberID                     string
	MaxNewIssuesPerDay, MaxOwnerWakesPerHour int
}

// loadConfig reads the repo's caps and owners; a repo without a row gets the column defaults.
func loadConfig(ctx context.Context, q queryer, a Action) (watchConfig, error) {
	c := watchConfig{MaxNewIssuesPerDay: 5, MaxOwnerWakesPerHour: 6}
	err := q.QueryRow(ctx, `SELECT COALESCE(fix_owner_kind,''), COALESCE(fix_owner_id::text,''),
			COALESCE(notify_member_id::text,''), COALESCE(authorized_by_member_id::text,''),
			max_new_issues_per_day, max_owner_wakes_per_hour
		FROM ci_watch_config WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3`,
		a.WorkspaceID, a.RepoOwner, a.RepoName).Scan(&c.FixOwnerKind, &c.FixOwnerID, &c.NotifyMemberID,
		&c.AuthorizedByMemberID, &c.MaxNewIssuesPerDay, &c.MaxOwnerWakesPerHour)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	return c, err
}

// lockRepoCaps serializes cap counting for a repo until the tx ends.
func lockRepoCaps(ctx context.Context, tx pgx.Tx, a Action) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		ownerKey(a.WorkspaceID, a.RepoOwner, a.RepoName, "#caps"))
	return err
}

// actor is the member a watcher issue or comment is attributed to, whose invocation
// permissions the wake is checked against, and who is accountable for the run. A
// notify recipient is not enough: that member must also have authorized the watcher
// for this repo (authorized_by_member_id), or anyone naming a member as recipient
// could run automation as them. A workspace-owner fallback is out for the same
// reason. Without a matching record the action fails closed.
func (c watchConfig) actor() (string, error) {
	if c.NotifyMemberID == "" {
		return "", &PermanentError{Reason: ReasonNoPrincipal, Err: errors.New("no notify_member_id configured for the watcher")}
	}
	if c.AuthorizedByMemberID != c.NotifyMemberID {
		return "", &PermanentError{Reason: ReasonNoPrincipal, Err: errors.New("the notify member has not authorized the watcher for this repo")}
	}
	return c.NotifyMemberID, nil
}

// requireMember fails closed unless user is still a member of the workspace. It runs
// in the write's own transaction and share-locks the member row, so a removal that
// races the write blocks until the write commits instead of slipping between the
// check and the write. An authorization record outlives the membership it was made
// under; this is the check that the record cannot answer.
func requireMember(ctx context.Context, tx pgx.Tx, workspaceID, user string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM member WHERE workspace_id=$1::uuid AND user_id=$2::uuid FOR SHARE`,
		workspaceID, user).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return &PermanentError{Reason: ReasonPrincipalRemoved, Err: errors.New("the watcher's member is no longer in the workspace")}
	}
	return err
}

// lockCheck serializes owners of one red-main check until the tx ends. Lock
// order is always signature, then check, as in admit.
func lockCheck(ctx context.Context, tx pgx.Tx, a Action, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		ownerKey(a.WorkspaceID, a.RepoOwner, a.RepoName, "#check:"+key))
	return err
}

type ownerState struct {
	Kind, State, IssueID, DigestClass, LastRunURL, CheckKey string
	FailCount                                               int
	LastDigestAt, ClosedAt                                  *time.Time
}

func readOwner(ctx context.Context, tx pgx.Tx, a Action) (o ownerState, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT kind, state, COALESCE(issue_id::text,''), digest_class, last_run_url, fail_count,
			last_digest_at, closed_at, check_key FROM ci_signature_owner
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4 FOR UPDATE`,
		a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash).Scan(&o.Kind, &o.State, &o.IssueID, &o.DigestClass,
		&o.LastRunURL, &o.FailCount, &o.LastDigestAt, &o.ClosedAt, &o.CheckKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, false, nil
	}
	return o, err == nil, err
}

type failureInfo struct {
	Workflow, Branch string
	Jobs             []JobEvidence
}

// readFailure tolerates a missing event: the watcher still files an issue with the signature.
func readFailure(ctx context.Context, tx pgx.Tx, a Action) (failureInfo, error) {
	var f failureInfo
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT workflow_name, head_branch, excerpts FROM ci_failure_event
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND run_id=$4
		ORDER BY run_attempt DESC LIMIT 1`, a.WorkspaceID, a.RepoOwner, a.RepoName, a.OriginalRunID).Scan(&f.Workflow, &f.Branch, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal(raw, &f.Jobs) // a malformed excerpt list is evidence loss, not a reason to skip the issue
	return f, nil
}

func shortSig(sig string) string {
	if len(sig) > 12 {
		return sig[:12]
	}
	return sig
}

// titleFor names the owner issue. A check with a key uses the red-main title,
// so every later run of that check finds the issue by its prefix.
func titleFor(kind, repo, checkKey string, f failureInfo, sig string) string {
	branch := f.Branch
	if branch == "" {
		branch = "main"
	}
	wf := EscapeName(f.Workflow)
	if checkKey != "" && kind != KindP3 {
		return fmt.Sprintf("%s %s red: %s: %s", redMainPrefix(repo, checkKey), EscapeName(branch), wf, shortSig(sig))
	}
	switch kind {
	case KindIncident:
		return fmt.Sprintf("incident(ci): %s red: %s: %s", EscapeName(branch), wf, shortSig(sig))
	case KindP3:
		return fmt.Sprintf("chore(ci): flaky: %s: %s", wf, shortSig(sig))
	}
	return fmt.Sprintf("fix(ci): %s red: %s: %s", EscapeName(branch), wf, shortSig(sig))
}

// takeWake spends one of the repo's hourly owner wakes. It reports false, with
// no event, when the cap is spent. The caller records ReasonWakeCapped.
func (l *Ledger) takeWake(ctx context.Context, tx pgx.Tx, a Action, cfg watchConfig) (bool, error) {
	if err := lockRepoCaps(ctx, tx, a); err != nil {
		return false, err
	}
	var wakes int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ci_watch_event WHERE workspace_id=$1::uuid AND repo_owner=$2
		AND repo_name=$3 AND kind='wake' AND created_at > $4`, a.WorkspaceID, a.RepoOwner, a.RepoName,
		l.now().Add(-time.Hour)).Scan(&wakes); err != nil {
		return false, err
	}
	if wakes >= cfg.MaxOwnerWakesPerHour {
		return false, l.noteReason(ctx, tx, a, ReasonWakeCapped) // settle keeps it: the issue exists, only the run was capped
	}
	_, err := tx.Exec(ctx, `INSERT INTO ci_watch_event (workspace_id, repo_owner, repo_name, signature_hash, kind,
		run_id, created_at) VALUES ($1::uuid,$2,$3,$4,'wake',$5,$6)`, a.WorkspaceID, a.RepoOwner, a.RepoName,
		a.SignatureHash, a.OriginalRunID, l.now())
	return err == nil, err
}

// bodyFor renders every untrusted field through EscapeName or UntrustedBlock.
func bodyFor(f failureInfo, sig, runURL string, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI failed on %s. Workflow: %s. Signature: %s.\n", EscapeName(f.Branch), EscapeName(f.Workflow), EscapeName(shortSig(sig)))
	if runURL != "" {
		fmt.Fprintf(&b, "Run: %s\nFailures so far: %d\n", EscapeName(runURL), count)
	}
	for _, j := range f.Jobs {
		fmt.Fprintf(&b, "\nFailed job: %s\n", EscapeName(j.Name))
		if j.Excerpt != "" {
			b.WriteString(UntrustedBlock(j.Excerpt) + "\n")
		}
	}
	return b.String()
}

func (l *Ledger) issueCreate(d Destinations) LocalEffect {
	return func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
		if d.Issue == nil {
			return "", nonTransactional("issue")
		}
		cfg, err := loadConfig(ctx, tx, a)
		if err != nil {
			return "", err
		}
		// First, so an unauthorized principal never takes the red-main claim.
		creator, err := cfg.actor()
		if err != nil {
			return "", err
		}
		if err := requireMember(ctx, tx, a.WorkspaceID, creator); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
			ownerKey(a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash)); err != nil {
			return "", err
		}
		o, found, err := readOwner(ctx, tx, a)
		if err != nil || !found {
			return "", errors.Join(err, errors.New("signature owner missing"))
		}
		// One owner per check, shared with red-main workers through the GitHub ref
		// claim. Without a usable key or a claimer the kind stays Shadow-only.
		if o.CheckKey == "" {
			return "", &PermanentError{Reason: ReasonNoCheckKey, Err: errors.New("no usable check key for the red-main claim")}
		}
		if d.RedMain == nil {
			return "", nonTransactional("red-main claim")
		}
		if err := lockCheck(ctx, tx, a, o.CheckKey); err != nil {
			return "", err
		}
		if ok, err := d.RedMain.ClaimCheck(ctx, tx, a.RepoOwner, a.RepoName, o.CheckKey, a.SignatureHash); err != nil {
			return "", err
		} else if !ok {
			return "", &PermanentError{Reason: ReasonRedMainClaimed, Err: errors.New("another holder owns the red-main check")}
		}
		f, err := readFailure(ctx, tx, a)
		if err != nil {
			return "", err
		}

		wake, err := l.takeWake(ctx, tx, a, cfg)
		if err != nil {
			return "", err
		}
		priority := "urgent"
		if o.Kind == KindP3 {
			priority = "medium"
		}
		id, err := d.Issue.CreateIssue(ctx, tx, IssueSpec{
			WorkspaceID: a.WorkspaceID, Title: titleFor(o.Kind, a.RepoName, o.CheckKey, f, a.SignatureHash),
			Body: bodyFor(f, a.SignatureHash, o.LastRunURL, o.FailCount), Priority: priority,
			AssigneeType: cfg.FixOwnerKind, AssigneeID: cfg.FixOwnerID, CreatorID: creator, Wake: wake,
			Metadata: map[string]any{"ci_watch_count": o.FailCount, "last_run_url": o.LastRunURL}, OriginID: a.ID,
		})
		if err != nil {
			return "", err
		}
		// The creation counts as the first digest, so the next one is 6 h away.
		_, err = tx.Exec(ctx, `UPDATE ci_signature_owner SET digest_class=$5, last_digest_at=$6
			WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`,
			a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, o.Kind, l.now())
		return id, err
	}
}

func (l *Ledger) issueComment(d Destinations) LocalEffect {
	return func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
		if d.Comment == nil {
			return "", nonTransactional("comment")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
			ownerKey(a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash)); err != nil {
			return "", err
		}
		o, found, err := readOwner(ctx, tx, a)
		if err != nil {
			return "", err
		}
		if !found || o.IssueID == "" {
			return "", l.noteReason(ctx, tx, a, "no_issue")
		}
		now := l.now()
		open := o.State == "open"
		classChange := open && o.DigestClass != o.Kind
		due := o.LastDigestAt == nil || now.Sub(*o.LastDigestAt) >= DigestEvery ||
			(o.State == "closed" && o.ClosedAt != nil && o.LastDigestAt.Before(*o.ClosedAt))
		if !classChange && !due {
			return "", l.noteReason(ctx, tx, a, "digest_coalesced")
		}
		cfg, err := loadConfig(ctx, tx, a)
		if err != nil {
			return "", err
		}
		author, err := cfg.actor()
		if err != nil {
			return "", err
		}
		if err := requireMember(ctx, tx, a.WorkspaceID, author); err != nil {
			return "", err
		}
		// An escalation (to incident, or from the low-key P3 issue to P2) raises
		// the priority and wakes the owner, under the same hourly cap as creation.
		if classChange && d.Issue != nil && (o.Kind == KindIncident || o.DigestClass == KindP3) {
			up := IssueUpdate{WorkspaceID: a.WorkspaceID, IssueID: o.IssueID, Priority: "urgent", PrincipalID: author}
			if o.Kind == KindIncident && o.CheckKey == "" {
				up.TitlePrefix = "incident(ci): "
			}
			if up.Wake, err = l.takeWake(ctx, tx, a, cfg); err != nil {
				return "", err
			}
			if err := d.Issue.UpdateIssue(ctx, tx, up); err != nil {
				return "", err
			}
		}
		id, err := d.Comment.AddComment(ctx, tx, CommentSpec{WorkspaceID: a.WorkspaceID, IssueID: o.IssueID, AuthorID: author,
			Body: digestBody(o, classChange)})
		if err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `UPDATE ci_signature_owner SET digest_class=$5, last_digest_at=$6
			WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`,
			a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, o.Kind, now)
		return id, err
	}
}

func digestBody(o ownerState, classChange bool) string {
	var b strings.Builder
	switch {
	case classChange:
		fmt.Fprintf(&b, "CI watch: this failure is now classified as %s.\n", EscapeName(o.Kind))
	case o.State == "closed":
		b.WriteString("CI watch: this signature failed again after the issue was closed. The issue stays closed.\n")
	default:
		b.WriteString("CI watch digest.\n")
	}
	fmt.Fprintf(&b, "Failures of this signature so far: %d\n", o.FailCount)
	if o.LastRunURL != "" {
		fmt.Fprintf(&b, "Latest run: %s\n", EscapeName(o.LastRunURL))
	}
	return b.String()
}

// noteReason records why a done action did nothing; settle keeps it.
func (l *Ledger) noteReason(ctx context.Context, tx pgx.Tx, a Action, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE ci_watch_action SET reason=$2 WHERE id=$1::uuid`, a.ID, reason)
	return err
}

func (l *Ledger) inboxNotify(d Destinations) LocalEffect {
	return func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
		if d.Inbox == nil {
			return "", nonTransactional("inbox")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO ci_watch_event (workspace_id, repo_owner, repo_name, signature_hash, kind,
			run_id, day, created_at) VALUES ($1::uuid,$2,$3,$4,'notify',$5, (($6::timestamptz) AT TIME ZONE 'UTC')::date, $6)
			ON CONFLICT (workspace_id, repo_owner, repo_name, signature_hash, day) WHERE kind = 'notify' DO NOTHING`,
			a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, a.OriginalRunID, l.now())
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return "", l.noteReason(ctx, tx, a, "deduped")
		}
		cfg, err := loadConfig(ctx, tx, a)
		if err != nil {
			return "", err
		}
		// A recipient, not a principal: the notification acts as nobody, so no authorization is needed.
		member := cfg.NotifyMemberID
		if member == "" {
			return "", &PermanentError{Reason: ReasonNoPrincipal, Err: errors.New("no notify_member_id configured for the watcher")}
		}
		if err := requireMember(ctx, tx, a.WorkspaceID, member); err != nil {
			return "", err
		}
		f, err := readFailure(ctx, tx, a)
		if err != nil {
			return "", err
		}
		var body strings.Builder
		fmt.Fprintf(&body, "CI needs a person: workflow %s, signature %s.\n", EscapeName(f.Workflow), EscapeName(shortSig(a.SignatureHash)))
		for _, j := range f.Jobs {
			fmt.Fprintf(&body, "\nFailed job: %s\n", EscapeName(j.Name))
			if j.Excerpt != "" {
				body.WriteString(UntrustedBlock(j.Excerpt) + "\n")
			}
		}
		return d.Inbox.Notify(ctx, tx, NotifySpec{WorkspaceID: a.WorkspaceID, MemberID: member,
			Title: "CI needs a person: " + EscapeName(f.Workflow), Body: body.String()})
	}
}
