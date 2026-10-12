package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func TestGovernanceCaseAuditEnabledFailsClosed(t *testing.T) {
	if GovernanceCaseAuditEnabled(context.Background(), nil) {
		t.Fatal("nil flag service enabled governance case audit")
	}
	if GovernanceCaseAuditEnabled(context.Background(), featureflag.NewService(nil)) {
		t.Fatal("missing flag provider enabled governance case audit")
	}
	if GovernanceCaseAuditEnabled(context.Background(), featureflag.NewService(featureflag.NewStaticProvider())) {
		t.Fatal("unconfigured governance case audit flag was not default-off")
	}

	provider := featureflag.NewStaticProvider()
	provider.LoadRules(map[string]featureflag.Rule{GovernanceCaseAudit: {Default: true}})
	if !GovernanceCaseAuditEnabled(context.Background(), featureflag.NewService(provider)) {
		t.Fatal("configured governance case audit flag did not enable the feature")
	}
}
