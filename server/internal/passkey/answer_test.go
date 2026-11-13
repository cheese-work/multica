package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (f *fixture) binding() AnswerBinding {
	return AnswerBinding{
		WorkspaceID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, DecisionID: f.action, AnswerRevision: 3, PolicyRevision: 2,
		RecipientUserID: f.user, SessionID: "sid-1", Option: "Option B", Text: "ship it",
	}
}

func (f *fixture) verifyAnswer(binding AnswerBinding, assertion json.RawMessage) (pgtype.UUID, error) {
	f.test.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credential, err := f.service.VerifyAnswer(ctx, db.New(tx), binding, assertion)
	if err != nil {
		return credential, err
	}
	if err := tx.Commit(ctx); err != nil {
		f.test.Fatal(err)
	}
	return credential, nil
}

func (f *fixture) issueAnswer(binding AnswerBinding) (Issued, string) {
	f.test.Helper()
	issued, err := f.service.IssueAnswer(context.Background(), binding)
	if err != nil {
		f.test.Fatal(err)
	}
	return issued, challengeOf(f.test, issued)
}

func TestAnswerDigestIsSha256OfCanonicalJSON(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	canonical := `{"answer_revision":3,"decision_id":"` + uuid.UUID(binding.DecisionID.Bytes).String() +
		`","option":"Option B","policy_revision":2,"recipient_user_id":"` + uuid.UUID(f.user.Bytes).String() +
		`","sid":"sid-1","text":"ship it <&>","v":1,"workspace_id":"` + uuid.UUID(binding.WorkspaceID.Bytes).String() + `"}`
	binding.Text = "ship it <&>"
	sum := sha256.Sum256([]byte(canonical))
	if got, want := binding.Digest(), hex.EncodeToString(sum[:]); got != want {
		test.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestAnswerDigestChangesWithEveryField(test *testing.T) {
	f := newFixture(test)
	base := f.binding()
	other := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	swapped := base
	swapped.Option, swapped.Text = base.Text, base.Option
	cases := map[string]func(*AnswerBinding){
		"workspace": func(b *AnswerBinding) { b.WorkspaceID = other },
		"decision":  func(b *AnswerBinding) { b.DecisionID = other },
		"answer":    func(b *AnswerBinding) { b.AnswerRevision++ },
		"policy":    func(b *AnswerBinding) { b.PolicyRevision++ },
		"recipient": func(b *AnswerBinding) { b.RecipientUserID = other },
		"sid":       func(b *AnswerBinding) { b.SessionID = "sid-2" },
		"option":    func(b *AnswerBinding) { b.Option = "Option A" },
		"text":      func(b *AnswerBinding) { b.Text = "ship it now" },
		"swap":      func(b *AnswerBinding) { *b = swapped },
		"boundary":  func(b *AnswerBinding) { b.Option, b.Text = "", "Option B\n\nship it" },
	}
	for name, change := range cases {
		changed := base
		change(&changed)
		if changed.Digest() == base.Digest() {
			test.Errorf("%s: digest did not change", name)
		}
	}
}

func TestVerifyAnswerAcceptsTheBoundContentOnceAndConsumes(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issueAnswer(binding)
	row, err := f.service.Queries.GetPasskeyChallenge(context.Background(), issued.ChallengeID)
	if err != nil {
		test.Fatal(err)
	}
	if row.Purpose != string(PurposeDecisionAnswer) || row.SubjectID != binding.DecisionID {
		test.Fatalf("challenge purpose/subject = %s/%v", row.Purpose, row.SubjectID)
	}
	assertion := f.sign(issued, challenge, startCount+1, nil)
	if credential, err := f.verifyAnswer(binding, assertion); err != nil || credential != f.key {
		test.Fatalf("verify = %v, %v", credential, err)
	}
	_, err = f.verifyAnswer(binding, f.sign(issued, challenge, startCount+2, nil))
	f.wantRefusal(err, 403, "assertion_rejected")
}

func TestVerifyAnswerRefusesAnyDifferentBoundContent(test *testing.T) {
	other := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	cases := map[string]func(*AnswerBinding){
		"workspace": func(b *AnswerBinding) { b.WorkspaceID = other },
		"decision":  func(b *AnswerBinding) { b.DecisionID = other },
		"answer":    func(b *AnswerBinding) { b.AnswerRevision++ },
		"policy":    func(b *AnswerBinding) { b.PolicyRevision++ },
		"sid":       func(b *AnswerBinding) { b.SessionID = "sid-2" },
		"option":    func(b *AnswerBinding) { b.Option = "Option A" },
		"text":      func(b *AnswerBinding) { b.Text = "ship it now" },
		"swap":      func(b *AnswerBinding) { b.Option, b.Text = b.Text, b.Option },
	}
	for name, change := range cases {
		test.Run(name, func(test *testing.T) {
			f := newFixture(test)
			binding := f.binding()
			issued, challenge := f.issueAnswer(binding)
			submitted := binding
			change(&submitted)
			_, err := f.verifyAnswer(submitted, f.sign(issued, challenge, startCount+1, nil))
			f.wantRefusal(err, 403, "assertion_rejected")
		})
	}
}

func TestVerifyAnswerRefusesAnotherUsersProof(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issueAnswer(binding)
	submitted := binding
	submitted.RecipientUserID = newUser(test, f.pool)
	_, err := f.verifyAnswer(submitted, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, 403, "assertion_rejected")
}

func TestVerifyAnswerRefusesExpiredAndSupersededProof(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issueAnswer(binding)
	f.pool.Exec(context.Background(), `UPDATE passkey_assertion_challenge SET expires_at = now() - interval '1 second' WHERE id = $1`, issued.ChallengeID)
	_, err := f.verifyAnswer(binding, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, 403, "challenge_expired")

	first, firstChallenge := f.issueAnswer(binding)
	f.issueAnswer(binding)
	_, err = f.verifyAnswer(binding, f.sign(first, firstChallenge, startCount+1, nil))
	f.wantRefusal(err, 403, "challenge_superseded")
}

func TestVerifyAnswerRefusesAnApprovalPurposeProof(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issue()
	_, err := f.verifyAnswer(binding, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, 403, "assertion_rejected")
}

func TestVerifyAnswerRolledBackAttemptKeepsTheProofUsable(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issueAnswer(binding)
	assertion := f.sign(issued, challenge, startCount+1, nil)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := f.service.VerifyAnswer(ctx, db.New(tx), binding, assertion); err != nil {
		test.Fatal(err)
	}
	tx.Rollback(ctx)
	if _, err := f.verifyAnswer(binding, assertion); err != nil {
		test.Fatalf("retry after rollback: %v", err)
	}
}

func TestVerifyAnswerTwoSimultaneousSubmissionsConsumeOnce(test *testing.T) {
	f := newFixture(test)
	binding := f.binding()
	issued, challenge := f.issueAnswer(binding)
	assertion := f.sign(issued, challenge, startCount+1, nil)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.verifyAnswer(binding, assertion)
			results <- err
		}()
	}
	accepted := 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		test.Fatalf("accepted = %d, want 1", accepted)
	}
}
