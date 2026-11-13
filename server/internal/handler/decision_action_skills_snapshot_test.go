package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

type skillSetFixture struct {
	agent    pgtype.UUID
	a, b     string // a is assigned, b is proposed only
	proposed []pgtype.UUID
}

func newSkillSetFixture(test *testing.T) skillSetFixture {
	test.Helper()
	ctx := context.Background()
	newSkill := func(name string) string {
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO skill (workspace_id, name, description, content) VALUES ($1, $2, 'd', 'c') RETURNING id`, testWorkspaceID, name+"-"+test.Name()).Scan(&id); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM skill WHERE id = $1`, id) })
		return id
	}
	fixture := skillSetFixture{a: newSkill("a"), b: newSkill("b")}
	agentID := createHandlerTestAgent(test, "skill-set-"+test.Name(), nil)
	fixture.agent = parseUUID(agentID)
	if _, err := testPool.Exec(ctx, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, agentID, fixture.a); err != nil {
		test.Fatal(err)
	}
	fixture.proposed = []pgtype.UUID{parseUUID(fixture.a), parseUUID(fixture.b)}
	return fixture
}

func (f skillSetFixture) digests(test *testing.T) (live, after string) {
	test.Helper()
	snapshot, err := loadSkillSetSnapshot(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), f.agent, f.proposed)
	if err != nil {
		test.Fatal(err)
	}
	return snapshot.LiveSHA256, snapshot.AfterSHA256
}

func TestSkillSetDigestChanges(test *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		change     func(f skillSetFixture) string // SQL run after the first digest
		live, post bool                           // which digest must change
	}{
		{"content", func(f skillSetFixture) string { return `UPDATE skill SET content = 'x' WHERE id = '` + f.a + `'` }, true, true},
		{"name", func(f skillSetFixture) string { return `UPDATE skill SET name = name || '-x' WHERE id = '` + f.a + `'` }, true, true},
		{"description", func(f skillSetFixture) string { return `UPDATE skill SET description = 'x' WHERE id = '` + f.a + `'` }, true, true},
		{"file add", func(f skillSetFixture) string {
			return `INSERT INTO skill_file (skill_id, path, content) VALUES ('` + f.a + `', 'n.md', 'n')`
		}, true, true},
		{"file edit", func(f skillSetFixture) string {
			return `INSERT INTO skill_file (skill_id, path, content) VALUES ('` + f.a + `', 'n.md', 'n') ON CONFLICT (skill_id, path) DO UPDATE SET content = 'm'`
		}, true, true},
		{"proposed-only skill file add", func(f skillSetFixture) string {
			return `INSERT INTO skill_file (skill_id, path, content) VALUES ('` + f.b + `', 'n.md', 'n')`
		}, false, true},
		{"proposed-only skill content", func(f skillSetFixture) string { return `UPDATE skill SET content = 'x' WHERE id = '` + f.b + `'` }, false, true},
		{"enabled flip", func(f skillSetFixture) string {
			return `UPDATE agent_skill SET enabled = FALSE WHERE skill_id = '` + f.a + `'`
		}, true, false},
	} {
		test.Run(tc.name, func(test *testing.T) {
			f := newSkillSetFixture(test)
			if tc.name == "file edit" {
				testPool.Exec(ctx, `INSERT INTO skill_file (skill_id, path, content) VALUES ($1, 'n.md', 'n')`, f.a)
			}
			live0, after0 := f.digests(test)
			if _, err := testPool.Exec(ctx, tc.change(f)); err != nil {
				test.Fatal(err)
			}
			live1, after1 := f.digests(test)
			if (live0 != live1) != tc.live || (after0 != after1) != tc.post {
				test.Fatalf("live changed=%v want %v; after changed=%v want %v", live0 != live1, tc.live, after0 != after1, tc.post)
			}
		})
	}
	test.Run("file remove", func(test *testing.T) {
		f := newSkillSetFixture(test)
		testPool.Exec(ctx, `INSERT INTO skill_file (skill_id, path, content) VALUES ($1, 'n.md', 'n')`, f.a)
		live0, after0 := f.digests(test)
		testPool.Exec(ctx, `DELETE FROM skill_file WHERE skill_id = $1`, f.a)
		live1, after1 := f.digests(test)
		if live0 == live1 || after0 == after1 {
			test.Fatal("digest did not change on file remove")
		}
	})
}

func TestSkillSetDigestStableAcrossOrderAndDuplicates(test *testing.T) {
	f := newSkillSetFixture(test)
	live0, after0 := f.digests(test)
	f.proposed = []pgtype.UUID{f.proposed[1], f.proposed[0], f.proposed[1]}
	live1, after1 := f.digests(test)
	if live0 != live1 || after0 != after1 {
		test.Fatal("digest depends on order or duplicates")
	}
}

func TestSkillSetSnapshotRefusesForeignSkill(test *testing.T) {
	ctx := context.Background()
	f := newSkillSetFixture(test)
	var foreignWorkspace, foreignSkill string
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Skill Set Foreign', 'skill-set-foreign', '', 'SSF') RETURNING id`).Scan(&foreignWorkspace); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, foreignWorkspace) })
	if err := testPool.QueryRow(ctx, `INSERT INTO skill (workspace_id, name) VALUES ($1, 'foreign') RETURNING id`, foreignWorkspace).Scan(&foreignSkill); err != nil {
		test.Fatal(err)
	}
	for name, id := range map[string]string{"foreign": foreignSkill, "missing": "00000000-0000-4000-8000-000000000001"} {
		_, err := loadSkillSetSnapshot(ctx, testHandler.Queries, parseUUID(testWorkspaceID), f.agent, append(f.proposed, parseUUID(id)))
		var refusal decisionIntentRefusal
		if !errors.As(err, &refusal) || refusal.status != http.StatusNotFound {
			test.Fatalf("%s skill: err = %v, want a 404 refusal", name, err)
		}
	}
}

func TestSkillSetSnapshotBusyWhileSkillRowIsWritten(test *testing.T) {
	f := newSkillSetFixture(test)
	holdRow(test, `SELECT id FROM skill WHERE id = $1 FOR NO KEY UPDATE`, f.b)
	_, err := loadSkillSetSnapshot(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), f.agent, f.proposed)
	if !errors.Is(err, errBusyRetry) {
		test.Fatalf("err = %v, want busy_retry", err)
	}
}
