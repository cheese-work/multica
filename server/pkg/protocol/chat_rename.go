package protocol

type ChatRenameResponse struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	PreviousTitle string `json:"previous_title"`
	MessageID     string `json:"message_id"`
}
