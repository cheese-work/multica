package main

// provider_recovery_env_compose_test.go — MULTICA_PROVIDER_RECOVERY_BASE_URL
// is read by router.go, but compose forwards only the variables its
// `environment:` block names. Without the line, setting it in .env leaves the
// provider-failure guard with no probe URL and skipped autopilots never
// recover (CHE-5731). The A/B overlay (deploy/cd/docker-compose.ab.yml)
// extends this backend service, so one line covers blue and green.

import "testing"

func TestProviderRecoveryBaseURLIsPassedIntoTheBackendContainer(t *testing.T) {
	t.Parallel()

	const name = "MULTICA_PROVIDER_RECOVERY_BASE_URL"
	compose := readRepoFile(t, "docker-compose.selfhost.yml")
	if !hasLinePrefix(compose, name+": ${"+name+":-}") {
		t.Fatalf("docker-compose.selfhost.yml backend environment: must contain `%s: ${%s:-}`.\n"+
			"Compose forwards only the variables it names, so setting %s in .env reaches the container empty and the guard never lifts.",
			name, name, name)
	}
}
