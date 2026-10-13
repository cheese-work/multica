package db

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestChatSessionProjectionDoesNotDependOnLaneSummary(test *testing.T) {
	for _, field := range []string{"LaneSummary", "LaneSummaryUpdatedAt"} {
		if _, exists := reflect.TypeFor[ChatSession]().FieldByName(field); exists {
			test.Errorf("retired field %s remains in the generated session model", field)
		}
	}
	queries, err := os.ReadFile("chat.sql.go")
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(queries), "lane_summary") {
		test.Fatal("generated chat queries still depend on the retired summary columns")
	}
}
