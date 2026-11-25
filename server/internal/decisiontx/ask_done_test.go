package decisiontx

import "testing"

func TestChoosesDone(t *testing.T) {
	for name, tc := range map[string]struct {
		option AskOption
		want   bool
	}{
		"option key done":    {AskOption{Label: "Set done", OptionKey: OptionKeyDone}, true},
		"typed status done":  {AskOption{AfterChoice: AfterServerAction, Action: &OptionAction{Type: ActionStatusSet, Params: ActionParams{Status: "done"}}}, true},
		"typed other status": {AskOption{AfterChoice: AfterServerAction, Action: &OptionAction{Type: ActionStatusSet, Params: ActionParams{Status: "blocked"}}}, false},
		"typed assign":       {AskOption{AfterChoice: AfterServerAction, Action: &OptionAction{Type: ActionAssign}}, false},
		"label alone":        {AskOption{Label: "Set done", AfterChoice: "record_only"}, false},
		"waiver key":         {AskOption{OptionKey: OptionKeyWaive}, false},
	} {
		if got := tc.option.ChoosesDone(); got != tc.want {
			t.Errorf("%s: ChoosesDone = %v, want %v", name, got, tc.want)
		}
	}
}

func TestOptionKeyDoneNeedsNoWaiverTopic(t *testing.T) {
	ask := Ask{Options: []AskOption{{Label: "Set done", OptionKey: OptionKeyDone}, {Label: "Leave open"}}}
	if err := ask.validateWaiverContract(); err != nil {
		t.Errorf("option_key done refused: %v", err)
	}
	ask.Options[1].OptionKey = "other"
	if err := ask.validateWaiverContract(); err == nil {
		t.Error("an unknown option_key was accepted without a topic")
	}
}
