package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestZZProbe(t *testing.T) {
	o := newOwnerFixture(t)
	o.Exec(t, "UPDATE agent SET archived_at = now() WHERE id = $1", o.agentID)
	for _, d := range []int{1000, 4000, 16000} {
		raw := strings.Repeat("[@", d) + "x" + strings.Repeat("](mention://agent/11111111-2222-3333-4444-555555555555)", d)
		s := time.Now()
		o.svc.TaskSvc.failureNoticeText(context.Background(), util.MustParseUUID(o.agentID), raw)
		t.Logf("depth %d: %s", d, time.Since(s))
	}
}
