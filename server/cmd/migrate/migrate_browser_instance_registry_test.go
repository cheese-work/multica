package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// browserInstanceRegistryVersions lists the migrations CHE-4781 (W3-18) owns, in up order. They
// run after W3-2's browserLeaseVersions and leave those files as they are.
var browserInstanceRegistryVersions = []string{
	"99964a_browser_instance_registry",
	"99964b_browser_gateway_credential_hash_idx",
}

// CHE-4781: browser_instance gets host, gateway and allowed classes; gateway credentials are
// stored per gateway. The host and class matrix (plan W1 r5) is a database constraint: C00 serves
// S2 and H, X99 serves S0, no exception.
func TestBrowserInstanceRegistryMigrationEnforcesHostClassMatrix(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "browser_registry_")
	pool := openTestPoolWithSearchPath(t, schema)
	all := slices.Concat(browserLeaseVersions, browserInstanceRegistryVersions)
	run := func(direction string) {
		t.Helper()
		versions := slices.Clone(all)
		if direction == "down" {
			slices.Reverse(versions)
		}
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, versions, direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
	}
	run("up")

	const ws = "00000000-0000-0000-0000-0000000000a1"
	insert := func(name, host, gateway string, classes []string) error {
		return execBrowserLease(t, ctx, pool, `
			INSERT INTO browser_instance (workspace_id, name, host, gateway, allowed_classes)
			VALUES ($1, $2, $3, $4, $5)`, ws, name, host, gateway, classes)
	}
	if err := insert("c00-1", "c00", "c00", []string{"S2", "H"}); err != nil {
		t.Errorf("c00-1: %v", err)
	}
	for _, name := range []string{"x99-1", "x99-2", "x99-3", "x99-4"} {
		if err := insert(name, "x99", "x99", []string{"S0"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Rows that W3-2 inserts without the new columns land on C00 (S2 and H).
	if err := execBrowserLease(t, ctx, pool, `INSERT INTO browser_instance (workspace_id, name) VALUES ($1, 'legacy')`, ws); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var host, gateway string
	var classes []string
	if err := pool.QueryRow(ctx, `SELECT host, gateway, allowed_classes FROM browser_instance WHERE name = 'legacy'`).Scan(&host, &gateway, &classes); err != nil {
		t.Fatal(err)
	}
	if host != "c00" || gateway != "c00" || !slices.Equal(classes, []string{"S2", "H"}) {
		t.Errorf("legacy defaults = %s %s %v, want c00 c00 [S2 H]", host, gateway, classes)
	}

	// The matrix has no exception.
	wantPgCode(t, insert("bad-1", "c00", "c00", []string{"S0"}), "23514", "S0 on C00")
	wantPgCode(t, insert("bad-2", "c00", "c00", []string{"S2", "S0"}), "23514", "S0 mixed into C00 classes")
	wantPgCode(t, insert("bad-3", "x99", "x99", []string{"S2"}), "23514", "S2 on X99")
	wantPgCode(t, insert("bad-4", "x99", "x99", []string{"H"}), "23514", "H on X99")
	wantPgCode(t, insert("bad-5", "x99", "x99", []string{}), "23514", "no allowed class")
	wantPgCode(t, insert("bad-6", "x99", "c00", []string{"S0"}), "23514", "gateway differs from host")
	wantPgCode(t, insert("bad-7", "mbp", "mbp", []string{"S0"}), "23514", "unknown host")
	wantPgCode(t, insert("bad-8", "x99", "x99", []string{"S1"}), "23514", "class S1")

	// One credential row per hash; a gateway is c00 or x99.
	cred := func(gateway, hash string) error {
		return execBrowserLease(t, ctx, pool, `INSERT INTO browser_gateway_credential (workspace_id, gateway, credential_hash) VALUES ($1, $2, $3)`, ws, gateway, hash)
	}
	if err := cred("c00", "hash-c00"); err != nil {
		t.Errorf("c00 credential: %v", err)
	}
	if err := cred("x99", "hash-x99"); err != nil {
		t.Errorf("x99 credential: %v", err)
	}
	wantPgCode(t, cred("x99", "hash-x99"), "23505", "duplicate credential hash")
	wantPgCode(t, cred("mbp", "hash-mbp"), "23514", "unknown gateway")

	run("down")
	for _, column := range []string{"host", "gateway", "allowed_classes"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'browser_instance' AND column_name = $2)`, schema, column).Scan(&exists); err != nil || exists {
			t.Errorf("column %s after down: exists=%v err=%v", column, exists, err)
		}
	}
	run("up") // re-runnable
}

// CHE-4781: a gateway credential resolves only to its own instances. Another gateway's instance,
// a revoked credential, an unknown credential and another workspace all return no row.
func TestBrowserGatewayCredentialResolvesOnlyOwnInstances(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "browser_cred_")
	pool := openTestPoolWithSearchPath(t, schema)
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, slices.Concat(browserLeaseVersions, browserInstanceRegistryVersions), "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
		Conditions:            conditionsForDirection("up"),
	}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	q := db.New(pool)
	ws := pgtype.UUID{Bytes: [16]byte{15: 0xa1}, Valid: true}
	otherWs := pgtype.UUID{Bytes: [16]byte{15: 0xa2}, Valid: true}

	register := func(name, host string, classes ...string) db.BrowserInstance {
		t.Helper()
		inst, err := q.RegisterBrowserInstance(ctx, db.RegisterBrowserInstanceParams{
			WorkspaceID: ws, Name: name, Host: host, Gateway: host, AllowedClasses: classes,
		})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return inst
	}
	c00 := register("c00-1", "c00", "S2", "H")
	x99 := []db.BrowserInstance{
		register("x99-1", "x99", "S0"),
		register("x99-2", "x99", "S0"),
	}
	// Registering the same name again updates it and keeps the id.
	if again := register("c00-1", "c00", "S2"); again.ID != c00.ID || !slices.Equal(again.AllowedClasses, []string{"S2"}) {
		t.Errorf("re-register c00-1 = %v %v, want id %v and [S2]", again.ID, again.AllowedClasses, c00.ID)
	}

	cred := func(workspace pgtype.UUID, gateway, hash string) {
		t.Helper()
		if _, err := q.CreateBrowserGatewayCredential(ctx, db.CreateBrowserGatewayCredentialParams{
			WorkspaceID: workspace, Gateway: gateway, CredentialHash: hash,
		}); err != nil {
			t.Fatalf("credential %s: %v", hash, err)
		}
	}
	cred(ws, "c00", "hash-c00")
	cred(ws, "x99", "hash-x99")
	cred(otherWs, "x99", "hash-other-ws")

	get := func(hash string, id pgtype.UUID) error {
		_, err := q.GetBrowserInstanceForGateway(ctx, db.GetBrowserInstanceForGatewayParams{CredentialHash: hash, ID: id})
		return err
	}
	if err := get("hash-c00", c00.ID); err != nil {
		t.Errorf("c00 credential, c00 instance: %v", err)
	}
	for _, inst := range x99 {
		if err := get("hash-x99", inst.ID); err != nil {
			t.Errorf("x99 credential, %s: %v", inst.Name, err)
		}
		if err := get("hash-c00", inst.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("c00 credential, %s: want no rows, got %v", inst.Name, err)
		}
	}
	if err := get("hash-x99", c00.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("x99 credential, c00-1: want no rows, got %v", err)
	}
	if err := get("hash-unknown", c00.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown credential: want no rows, got %v", err)
	}
	if err := get("hash-other-ws", x99[0].ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another workspace's credential: want no rows, got %v", err)
	}

	list, err := q.ListBrowserInstancesForGateway(ctx, "hash-x99")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "x99-1" || list[1].Name != "x99-2" {
		t.Errorf("x99 list = %v, want x99-1 and x99-2 only", list)
	}

	if _, err := pool.Exec(ctx, `UPDATE browser_gateway_credential SET revoked_at = now() WHERE credential_hash = 'hash-x99'`); err != nil {
		t.Fatal(err)
	}
	if err := get("hash-x99", x99[0].ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("revoked credential: want no rows, got %v", err)
	}
}
