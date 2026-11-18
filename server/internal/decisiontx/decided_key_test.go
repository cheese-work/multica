package decisiontx

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// wantExcludedAskFields is the whole exclusion list of CHE-5682 section 2: the
// asker's explanation, checks and provenance. state_claims (CHE-5950) is not in
// it: a changed live fact is a different ask. Changing KeyExcludedAskFields must
// change this list.
var wantExcludedAskFields = []string{
	"context_eli10", "agent_did", "stakes_if_wrong", "recommendation", "recommended_option", "confidence", "probes",
	"source_issue_id", "source_comment_id",
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func keyOf(test *testing.T, ask Ask, action *ActionKeyInput) string {
	test.Helper()
	key, err := MatchKey(KindDecision, "none", ask, action)
	if err != nil {
		test.Fatalf("MatchKey: %v", err)
	}
	return key
}

func testAction() ActionKeyInput {
	return ActionKeyInput{
		Type: "workflow.step.instructions.set", Version: 1, Params: json.RawMessage(`{"workflow_id":"w1","step":"todo","n":2}`),
		SecretSlots: []decisionaction.SecretSlot{
			{Slot: "token", Target: decisionaction.SlotTarget{AgentID: "a1", EnvKey: "TOKEN"}},
			{Slot: "other", Target: decisionaction.SlotTarget{AgentID: "a2", EnvKey: "OTHER"}},
		},
	}
}

// --- reflection helpers: a field added to Ask or AskOption is tested with no change here.

func jsonName(field reflect.StructField) string {
	return strings.Split(field.Tag.Get("json"), ",")[0]
}

func fill(value reflect.Value, seed string) {
	switch value.Kind() {
	case reflect.String:
		value.SetString(seed)
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Float64:
		value.SetFloat(0.5)
	case reflect.Int, reflect.Int32, reflect.Int64:
		value.SetInt(7)
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fill(value.Elem(), seed)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		fill(value.Index(0), seed)
	case reflect.Struct:
		for i := range value.NumField() {
			field := value.Type().Field(i)
			if field.IsExported() && jsonName(field) != "-" {
				fill(value.Field(i), seed+"."+field.Name)
			}
		}
	default:
		panic("fill: unsupported kind " + value.Kind().String())
	}
}

func filledAsk() Ask {
	var ask Ask
	fill(reflect.ValueOf(&ask).Elem(), "v")
	return ask
}

// leafPaths lists the Go field path of every leaf field of a struct, through
// pointers and slices of structs.
func leafPaths(structType reflect.Type) [][]string {
	var paths [][]string
	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() || jsonName(field) == "-" {
			continue
		}
		inner := field.Type
		for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Slice {
			inner = inner.Elem()
		}
		if inner.Kind() != reflect.Struct {
			paths = append(paths, []string{field.Name})
			continue
		}
		for _, sub := range leafPaths(inner) {
			paths = append(paths, append([]string{field.Name}, sub...))
		}
	}
	return paths
}

func navigate(value reflect.Value, path []string) reflect.Value {
	for _, name := range path {
		for value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice {
			if value.Kind() == reflect.Pointer {
				value = value.Elem()
			} else {
				value = value.Index(0)
			}
		}
		value = value.FieldByName(name)
	}
	return value
}

// change alters one leaf so its meaning differs, not only its spacing.
func change(leaf reflect.Value) {
	switch leaf.Kind() {
	case reflect.String:
		leaf.SetString(leaf.String() + " changed")
	case reflect.Bool:
		leaf.SetBool(!leaf.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		leaf.SetInt(leaf.Int() + 1)
	case reflect.Float64:
		leaf.SetFloat(leaf.Float() + 0.25)
	case reflect.Pointer:
		next := reflect.New(leaf.Type().Elem())
		fill(next.Elem(), "changed")
		leaf.Set(next)
	case reflect.Slice:
		extra := reflect.New(leaf.Type().Elem()).Elem()
		fill(extra, "changed")
		leaf.Set(reflect.Append(leaf, extra))
	default:
		panic("change: unsupported kind " + leaf.Kind().String())
	}
}

func askJSONNameOf(test *testing.T, goName string) string {
	test.Helper()
	field, ok := reflect.TypeOf(Ask{}).FieldByName(goName)
	if !ok {
		test.Fatalf("no Ask field %s", goName)
	}
	return jsonName(field)
}

func TestKeyExclusionListIsExactlyTheNamedOne(test *testing.T) {
	got := slices.Clone(KeyExcludedAskFields)
	want := slices.Clone(wantExcludedAskFields)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		test.Fatalf("KeyExcludedAskFields = %v, want %v", got, want)
	}
	known := map[string]bool{}
	for i := range reflect.TypeOf(Ask{}).NumField() {
		known[jsonName(reflect.TypeOf(Ask{}).Field(i))] = true
	}
	for _, name := range want {
		if !known[name] {
			test.Fatalf("excluded field %q is not an Ask field", name)
		}
	}
}

// CHE-5682 test 11: same labels, one field changed alone. In-key fields give a
// different key; each excluded field changed alone still matches.
func TestMatchKeyChangeOfEachAskFieldAlone(test *testing.T) {
	excluded := map[string]bool{}
	for _, name := range wantExcludedAskFields {
		excluded[name] = true
	}
	paths := leafPaths(reflect.TypeOf(Ask{}))
	if len(paths) < 30 {
		test.Fatalf("reflection found only %d Ask leaf fields", len(paths))
	}
	base := keyOf(test, filledAsk(), nil)
	for _, path := range paths {
		test.Run(strings.Join(path, "."), func(test *testing.T) {
			ask := filledAsk()
			change(navigate(reflect.ValueOf(&ask).Elem(), path))
			got := keyOf(test, ask, nil)
			if isExcluded := excluded[askJSONNameOf(test, path[0])]; isExcluded && got != base {
				test.Fatalf("excluded field %v changed the key", path)
			} else if !isExcluded && got == base {
				test.Fatalf("in-key field %v did not change the key", path)
			}
		})
	}
}

// Each state_claims field changed alone, and a claim added, changes the key.
func TestMatchKeyChangeOfEachStateClaimFieldAlone(test *testing.T) {
	paths := leafPaths(reflect.TypeOf(StateClaim{}))
	if len(paths) != 3 {
		test.Fatalf("StateClaim leaf fields = %d, want 3; a new field must be reviewed", len(paths))
	}
	base := keyOf(test, filledAsk(), nil)
	for _, path := range paths {
		test.Run(strings.Join(path, "."), func(test *testing.T) {
			ask := filledAsk()
			change(navigate(reflect.ValueOf(&ask.StateClaims[0]).Elem(), path))
			if got := keyOf(test, ask, nil); got == base {
				test.Fatalf("state claim field %v did not change the key", path)
			}
		})
	}
	test.Run("claim added", func(test *testing.T) {
		ask := filledAsk()
		ask.StateClaims = append(ask.StateClaims, ask.StateClaims[0])
		if got := keyOf(test, ask, nil); got == base {
			test.Fatal("an added state claim did not change the key")
		}
	})
	test.Run("no claims", func(test *testing.T) {
		ask := filledAsk()
		ask.StateClaims = nil
		if got := keyOf(test, ask, nil); got == base {
			test.Fatal("dropping the state claims did not change the key")
		}
	})
}

// CHE-5682 test 11, option half: every AskOption field changed alone (the label
// set stays the same except for the label field itself).
func TestMatchKeyChangeOfEachOptionFieldAlone(test *testing.T) {
	paths := leafPaths(reflect.TypeOf(AskOption{}))
	if len(paths) != 9 {
		test.Fatalf("AskOption leaf fields = %d, want 9; a new field must be reviewed", len(paths))
	}
	base := keyOf(test, filledAsk(), nil)
	for _, path := range paths {
		test.Run(strings.Join(path, "."), func(test *testing.T) {
			ask := filledAsk()
			change(navigate(reflect.ValueOf(&ask.Options[0]).Elem(), path))
			if got := keyOf(test, ask, nil); got == base {
				test.Fatalf("option field %v did not change the key", path)
			}
		})
	}
}

// CHE-5682 test 12.
func TestMatchKeyActionParams(test *testing.T) {
	ask := decodedAsk(test)
	action := testAction()
	base := keyOf(test, ask, &action)

	changed := testAction()
	changed.Params = json.RawMessage(`{"workflow_id":"w1","step":"todo","n":3}`)
	if keyOf(test, ask, &changed) == base {
		test.Fatal("changed params gave the same key")
	}
	reordered := testAction()
	reordered.Params = json.RawMessage(" {\n \"n\": 2,\t\"step\":\"todo\",\n\"workflow_id\" : \"w1\" }\n")
	if keyOf(test, ask, &reordered) != base {
		test.Fatal("params key order and spacing changed the key (RFC 8785 form expected)")
	}
}

// CHE-5682 test 13.
func TestMatchKeyActionTypeVersionSlots(test *testing.T) {
	ask := decodedAsk(test)
	action := testAction()
	base := keyOf(test, ask, &action)
	cases := map[string]func(*ActionKeyInput){
		"type":         func(a *ActionKeyInput) { a.Type = "agent.instructions.set" },
		"version":      func(a *ActionKeyInput) { a.Version = 2 },
		"slot name":    func(a *ActionKeyInput) { a.SecretSlots[0].Slot = "token2" },
		"slot agent":   func(a *ActionKeyInput) { a.SecretSlots[0].Target.AgentID = "a9" },
		"slot env key": func(a *ActionKeyInput) { a.SecretSlots[0].Target.EnvKey = "TOKEN2" },
		"slot removed": func(a *ActionKeyInput) { a.SecretSlots = a.SecretSlots[:1] },
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			changed := testAction()
			mutate(&changed)
			if keyOf(test, ask, &changed) == base {
				test.Fatalf("changed %s gave the same key", name)
			}
		})
	}
	test.Run("slot order is not identity", func(test *testing.T) {
		swapped := testAction()
		swapped.SecretSlots[0], swapped.SecretSlots[1] = swapped.SecretSlots[1], swapped.SecretSlots[0]
		if keyOf(test, ask, &swapped) != base {
			test.Fatal("slot order changed the key; slots are sorted")
		}
	})
	test.Run("nil and empty slots match", func(test *testing.T) {
		none, empty := testAction(), testAction()
		none.SecretSlots, empty.SecretSlots = nil, []decisionaction.SecretSlot{}
		if keyOf(test, ask, &none) != keyOf(test, ask, &empty) {
			test.Fatal("nil and empty slots differ")
		}
	})
}

// CHE-5682 test 14.
func TestMatchKeyActionPresentVersusAbsent(test *testing.T) {
	ask := decodedAsk(test)
	action := testAction()
	if keyOf(test, ask, &action) == keyOf(test, ask, nil) {
		test.Fatal("an ask with an action matches the same ask without one")
	}
	doc, err := matchKeyDoc(KindDecision, "none", ask, nil)
	if err != nil || !strings.Contains(string(doc), `"action":null`) {
		test.Fatalf("absent action must be null in the key document: %s (%v)", doc, err)
	}
}

func TestMatchKeyRefusesWhatItCannotCanonicalize(test *testing.T) {
	ask := decodedAsk(test)
	for name, params := range map[string]string{"not json": `{`, "fraction": `{"n":1.5}`, "duplicate key": `{"a":1,"a":2}`, "empty": ``} {
		test.Run(name, func(test *testing.T) {
			action := testAction()
			action.Params = json.RawMessage(params)
			if key, err := MatchKey(KindDecision, "none", ask, &action); err == nil || key != "" {
				test.Fatalf("key = %q, err = %v; want an error and no key", key, err)
			}
		})
	}
}

func TestMatchKeyShapeAndVersion(test *testing.T) {
	ask := decodedAsk(test)
	if key := keyOf(test, ask, nil); !hex64.MatchString(key) {
		test.Fatalf("key %q is not 64 lowercase hex characters", key)
	}
	doc, err := matchKeyDoc(KindDecision, "none", ask, nil)
	if err != nil || !strings.Contains(string(doc), `"key_version":"v1"`) {
		test.Fatalf("key document lacks key_version v1: %s (%v)", doc, err)
	}
	if KeyVersion != "v1" {
		test.Fatalf("KeyVersion = %q", KeyVersion)
	}
}

func TestMatchKeyKindAndClass(test *testing.T) {
	ask := decodedAsk(test)
	base, _ := MatchKey(KindDecision, "none", ask, nil)
	for name, got := range map[string]func() (string, error){
		"kind":  func() (string, error) { return MatchKey(KindAction, "none", ask, nil) },
		"class": func() (string, error) { return MatchKey(KindDecision, "spend", ask, nil) },
	} {
		key, err := got()
		if err != nil || key == base {
			test.Fatalf("%s: key %q err %v; want a different key", name, key, err)
		}
	}
}

// Canonical form: option reorder, whitespace, exact-byte fields, list order, nil lists.
func TestMatchKeyCanonicalForm(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Ask)
		same   bool
	}{
		{"options reordered", func(a *Ask) { a.Options[0], a.Options[1] = a.Options[1], a.Options[0] }, true},
		{"string trimmed", func(a *Ask) { a.Decision = "  " + a.Decision + "\n" }, true},
		{"whitespace collapsed", func(a *Ask) { a.Options[0].WhatHappens = strings.ReplaceAll(a.Options[0].WhatHappens, " ", " \t\n  ") }, true},
		{"pro whitespace collapsed", func(a *Ask) { a.Options[0].Pros[0] = "  " + a.Options[0].Pros[0] + "   " }, true},
		{"nil and empty human steps", func(a *Ask) { a.HumanSteps = []HumanStep{} }, true},
		{"asked by another agent", func(a *Ask) { a.AskedBy = "someone-else" }, true},
		{"kind field is not caller input", func(a *Ask) { a.Kind = KindAction }, true},
		{"words changed", func(a *Ask) { a.Decision += " now" }, false},
		{"pros reordered", func(a *Ask) { a.Options[0].Pros = []string{"B", "A"} }, false},
		{"internal word space is one space, not none", func(a *Ask) { a.Decision = strings.ReplaceAll(a.Decision, " ", "") }, false},
		{"agent command spacing is exact", func(a *Ask) { a.Options[0].AgentCommands = [][]string{{"multica", "issue  status"}} }, false},
		{"human step command spacing is exact", func(a *Ask) { a.HumanSteps = []HumanStep{{Action: "Run", Command: "ls  -l", VerifyDone: "Done"}} }, false},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			base := decodedAsk(test)
			base.Options[0].AgentCommands = [][]string{{"multica", "issue status"}}
			base.HumanSteps = []HumanStep{{Action: "Run", Command: "ls -l", VerifyDone: "Done"}}
			if tc.name == "nil and empty human steps" {
				base.HumanSteps = nil
			}
			changed := base
			changed.Options = slices.Clone(base.Options)
			for i := range changed.Options {
				changed.Options[i].Pros = slices.Clone(changed.Options[i].Pros)
			}
			tc.mutate(&changed)
			got := keyOf(test, changed, nil) == keyOf(test, base, nil)
			if got != tc.same {
				test.Fatalf("keys equal = %v, want %v", got, tc.same)
			}
		})
	}
}

func TestSubjectKey(test *testing.T) {
	ask := decodedAsk(test)
	ask.Subject, ask.Topic = "Release 1.2", "ship"
	base, err := SubjectKey(KindDecision, ask)
	if err != nil || !hex64.MatchString(base) {
		test.Fatalf("subject key %q err %v", base, err)
	}
	same := map[string]func(*Ask){
		"another asker":    func(a *Ask) { a.AskedBy = "other" },
		"other options":    func(a *Ask) { a.Options = a.Options[:1] },
		"other prose":      func(a *Ask) { a.ContextELI10, a.Recommendation = "x", "y" },
		"decision spacing": func(a *Ask) { a.Decision = " " + strings.ReplaceAll(a.Decision, " ", "  ") },
	}
	for name, mutate := range same {
		changed := ask
		mutate(&changed)
		if got, _ := SubjectKey(KindDecision, changed); got != base {
			test.Fatalf("%s changed the subject key", name)
		}
	}
	differs := map[string]func(*Ask){
		"decision": func(a *Ask) { a.Decision += "?" },
		"subject":  func(a *Ask) { a.Subject = "Release 1.3" },
		"topic":    func(a *Ask) { a.Topic = "hold" },
	}
	for name, mutate := range differs {
		changed := ask
		mutate(&changed)
		if got, _ := SubjectKey(KindDecision, changed); got == base {
			test.Fatalf("%s did not change the subject key", name)
		}
	}
	if got, _ := SubjectKey(KindAction, ask); got == base {
		test.Fatal("kind did not change the subject key")
	}
	if match, _ := MatchKey(KindDecision, "none", ask, nil); match == base {
		test.Fatal("subject key equals match key")
	}
}

func TestProposalKey(test *testing.T) {
	action := testAction()
	base, err := ProposalKey(action)
	if err != nil || !hex64.MatchString(base) {
		test.Fatalf("proposal key %q err %v", base, err)
	}
	reordered := testAction()
	reordered.Params = json.RawMessage(`{ "n":2, "step":"todo", "workflow_id":"w1"}`)
	if got, _ := ProposalKey(reordered); got != base {
		test.Fatal("params key order changed the proposal key")
	}
	for name, mutate := range map[string]func(*ActionKeyInput){
		"type":    func(a *ActionKeyInput) { a.Type = "x" },
		"version": func(a *ActionKeyInput) { a.Version = 9 },
		"params":  func(a *ActionKeyInput) { a.Params = json.RawMessage(`{"n":3}`) },
		"slots":   func(a *ActionKeyInput) { a.SecretSlots = nil },
	} {
		changed := testAction()
		mutate(&changed)
		if got, _ := ProposalKey(changed); got == base {
			test.Fatalf("%s did not change the proposal key", name)
		}
	}
	bad := testAction()
	bad.Params = json.RawMessage(`{`)
	if key, err := ProposalKey(bad); err == nil || key != "" {
		test.Fatalf("invalid params: key %q err %v", key, err)
	}
}
