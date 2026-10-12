package middleware

import (
	"log"
	"os"
	"testing"
)

func TestMain(tests *testing.M) {
	if err := os.Setenv("AUTH_TOKEN_TTL", "1h"); err != nil {
		log.Fatal(err)
	}
	os.Exit(tests.Run())
}
