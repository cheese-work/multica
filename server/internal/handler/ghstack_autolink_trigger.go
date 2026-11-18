package handler

import (
	"github.com/multica-ai/multica/server/internal/integrations/ghstack"
)

// enqueueStackLink queues a stack-link job for the PR events that can change
// how it chains: opened, reopened, ready_for_review, synchronize, and edited
// with a base change (CHE-4143 step 7). A merge goes through
// refreshOpenPRsOnBase instead. It never fails the delivery: the queue is
// non-blocking, and whether the PR is linked to an issue, the mode and every
// GitHub failure are settled in the background job.
func (h *Handler) enqueueStackLink(p *ghPullRequestPayload) {
	switch p.Action {
	case "opened", "reopened", "ready_for_review", "synchronize":
	case "edited":
		if !baseRefChanged(p.Changes) {
			return
		}
	default:
		return
	}
	h.enqueueStackLinkNumber(p.Installation.ID, p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number)
}

// enqueueStackLinkNumber queues the job for one PR number of a repo.
func (h *Handler) enqueueStackLinkNumber(installationID int64, owner, repo string, number int32) {
	if h.StackAutolink == nil {
		return
	}
	h.StackAutolink.Enqueue(ghstack.Job{InstallationID: installationID, Owner: owner, Repo: repo, PR: int(number)})
}
