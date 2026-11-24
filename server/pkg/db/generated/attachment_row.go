package db

// Attachment converts the sqlc row returned by the CTE-backed create query
// into the canonical model used by the rest of the application.
func (r CreateAttachmentRow) Attachment() Attachment {
	return Attachment{
		ID:            r.ID,
		WorkspaceID:   r.WorkspaceID,
		IssueID:       r.IssueID,
		CommentID:     r.CommentID,
		UploaderType:  r.UploaderType,
		UploaderID:    r.UploaderID,
		Filename:      r.Filename,
		Url:           r.Url,
		ContentType:   r.ContentType,
		SizeBytes:     r.SizeBytes,
		CreatedAt:     r.CreatedAt,
		ChatSessionID: r.ChatSessionID,
		ChatMessageID: r.ChatMessageID,
		TaskID:        r.TaskID,
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
		Revision:      r.Revision,
		RevisionOf:    r.RevisionOf,
		RevisionRoot:  r.RevisionRoot,
		Sha256:        r.Sha256,
||||||| 2ae2f08005
=======
		Width:         r.Width,
		Height:        r.Height,
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
	}
}
