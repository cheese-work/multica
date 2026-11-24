package decisiontx

// ChoosesDone reports whether picking the option closes the issue as done: it
// carries option_key done, or a typed issue.status.set to done (CHE-9259).
func (option AskOption) ChoosesDone() bool {
	if option.OptionKey == OptionKeyDone {
		return true
	}
	return option.Action != nil && option.Action.Type == ActionStatusSet && option.Action.Params.Status == "done"
}
