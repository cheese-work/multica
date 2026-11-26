package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

// SubjectArtifactApproval marks an ask that approves a file; it needs artifacts.
const SubjectArtifactApproval = "artifact_approval"

// SubjectDiscoveryBrief marks the Discovery brief card (Plan step 13, CHE-6107 A5). It is
// class reversible by contract; the reversible ask guard exempts this subject and no other (CHE-10271).
const SubjectDiscoveryBrief = "discovery_brief"

// Discovery brief answer labels (Plan step 13): the brief offers exactly these two.
const (
	BriefOptionContinue = "Tiếp tục"
	BriefOptionStop     = "Dừng"
)

// IsDiscoveryBrief reports whether the ask has the Discovery brief shape: subject
// discovery_brief, no artifacts, and exactly the two options "Tiếp tục" and "Dừng".
// A relabelled ordinary approval has other options or artifacts and does not match.
func (ask Ask) IsDiscoveryBrief() bool {
	if ask.Subject != SubjectDiscoveryBrief || len(ask.Artifacts) != 0 || len(ask.Options) != 2 {
		return false
	}
	labels := []string{ask.Options[0].Label, ask.Options[1].Label}
	return slices.Contains(labels, BriefOptionContinue) && slices.Contains(labels, BriefOptionStop)
}

var (
	artifactFields  = []string{"attachment_id", "label", "sha256", "supersedes_attachment_id", "signed_by", "supersedes_signed_by"}
	signatureFields = []string{"reviewer", "comment_id"}
	sha256Hex       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func decodeArtifacts(raw json.RawMessage, into *[]Artifact) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{"ask.artifacts", "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("ask.artifacts[%d]", index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, "must be an object with attachment_id, label, sha256"}
		}
		if err := rejectUnknown(prefix, fields, artifactFields); err != nil {
			return err
		}
		var artifact Artifact
		for _, field := range []struct {
			name string
			into *string
		}{{"attachment_id", &artifact.AttachmentID}, {"label", &artifact.Label}, {"sha256", &artifact.SHA256}, {"supersedes_attachment_id", &artifact.SupersedesAttachmentID}} {
			if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
				return err
			}
		}
		for _, field := range []struct {
			name string
			into **ArtifactSignature
		}{{"signed_by", &artifact.SignedBy}, {"supersedes_signed_by", &artifact.SupersedesSignedBy}} {
			if raw := fields[field.name]; len(raw) > 0 {
				signature, err := decodeSignature(prefix+"."+field.name, raw)
				if err != nil {
					return err
				}
				*field.into = signature
			}
		}
		*into = append(*into, artifact)
	}
	return nil
}

func decodeSignature(prefix string, raw json.RawMessage) (*ArtifactSignature, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &AskError{prefix, "must be an object with reviewer, comment_id"}
	}
	if err := rejectUnknown(prefix, fields, signatureFields); err != nil {
		return nil, err
	}
	var signature ArtifactSignature
	for _, field := range []struct {
		name string
		into *string
	}{{"reviewer", &signature.Reviewer}, {"comment_id", &signature.CommentID}} {
		if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
			return nil, err
		}
	}
	return &signature, nil
}

func (ask Ask) validateArtifacts() error {
	switch ask.Subject {
	case "", SubjectArtifactApproval, SubjectDiscoveryBrief:
	default:
		return &AskError{"ask.subject", "must be " + SubjectArtifactApproval + " or " + SubjectDiscoveryBrief + " when set"}
	}
	if ask.Subject == SubjectArtifactApproval && len(ask.Artifacts) == 0 {
		return &AskError{"ask.artifacts", "requires at least one artifact when subject is " + SubjectArtifactApproval}
	}
	seen := make(map[string]bool)
	for index, artifact := range ask.Artifacts {
		prefix := fmt.Sprintf("ask.artifacts[%d]", index)
		if _, err := util.ParseUUID(artifact.AttachmentID); err != nil {
			return &AskError{prefix + ".attachment_id", "must be a UUID"}
		}
		if seen[artifact.AttachmentID] {
			return &AskError{prefix + ".attachment_id", "must differ from the other artifacts"}
		}
		seen[artifact.AttachmentID] = true
		if strings.TrimSpace(artifact.Label) == "" {
			return &AskError{prefix + ".label", "is required"}
		}
		if !sha256Hex.MatchString(artifact.SHA256) {
			return &AskError{prefix + ".sha256", "must be 64 lowercase hex characters"}
		}
		if artifact.SupersedesAttachmentID != "" {
			if _, err := util.ParseUUID(artifact.SupersedesAttachmentID); err != nil {
				return &AskError{prefix + ".supersedes_attachment_id", "must be a UUID"}
			}
		}
		if artifact.SupersedesSignedBy != nil && artifact.SupersedesAttachmentID == "" {
			return &AskError{prefix + ".supersedes_signed_by", "requires supersedes_attachment_id"}
		}
		for _, field := range []struct {
			name string
			from *ArtifactSignature
		}{{"signed_by", artifact.SignedBy}, {"supersedes_signed_by", artifact.SupersedesSignedBy}} {
			if field.from == nil {
				continue
			}
			if strings.TrimSpace(field.from.Reviewer) == "" {
				return &AskError{prefix + "." + field.name + ".reviewer", "is required"}
			}
			if _, err := util.ParseUUID(field.from.CommentID); err != nil {
				return &AskError{prefix + "." + field.name + ".comment_id", "must be a UUID"}
			}
		}
	}
	return nil
}
