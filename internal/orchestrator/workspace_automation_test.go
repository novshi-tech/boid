package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const workspaceAutomationYAML = `triggers:
  - name: sweep
    every: 1m
    run: echo sweep
signals:
  sources:
    - connector: demo/poll
      service: demo
      every: 2m
card_commands:
  z-last:
    label: Last
    run: echo last
  judge:
    label: Custom
    run: echo custom
card_events:
  command: ' judge '
`

func automationMeta(t *testing.T) *WorkspaceMeta {
	t.Helper()
	var meta WorkspaceMeta
	if err := yaml.Unmarshal([]byte(workspaceAutomationYAML), &meta); err != nil {
		t.Fatal(err)
	}
	return &meta
}

func TestWorkspaceAutomationPersistenceAndEnvelopeRoundTrip(t *testing.T) {
	repo := newTestWorkspaceRepo(t)
	meta := automationMeta(t)
	if err := repo.Create("team", meta); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"create", "save", "cas"} {
		if write == "save" {
			if err := repo.Save("team", meta); err != nil {
				t.Fatal(err)
			}
		}
		if write == "cas" {
			_, rev, err := repo.LoadWithRevision("team")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := repo.UpdateIfRevisionMatches("team", rev, meta); err != nil || !ok {
				t.Fatalf("cas: %v %v", ok, err)
			}
		}
		got, err := repo.Load("team")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Triggers, meta.Triggers) || !reflect.DeepEqual(got.Signals, meta.Signals) || !reflect.DeepEqual(got.CardCommands, meta.CardCommands) || !reflect.DeepEqual(got.CardCommandsOrder, []string{"z-last", "judge"}) || got.CardEvents.Command != "judge" {
			t.Fatalf("%s: %+v", write, got)
		}
		data, err := yaml.Marshal(NewWorkspaceEnvelopeFromMeta("team", got, nil, ""))
		if err != nil {
			t.Fatal(err)
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Fatal(err)
		}
		restored := docs[0].MergeInto(nil)
		if !reflect.DeepEqual(restored.CardCommandsOrder, got.CardCommandsOrder) || !reflect.DeepEqual(restored.Triggers, got.Triggers) || !reflect.DeepEqual(restored.Signals, got.Signals) || restored.CardEvents != got.CardEvents {
			t.Fatalf("envelope: %+v", restored)
		}
	}
}

func TestWorkspaceAutomationReceiverOnlyAndProjectPriority(t *testing.T) {
	ws := NewWorkspaceStore(t.TempDir())
	if err := ws.Save("team", automationMeta(t)); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(ws)
	receiver := DefaultMetaprojectID("team")
	store.SetSynthesizedMeta(receiver, DefaultMetaprojectMeta("team"))
	store.SetWorkspaceID(receiver, "team")
	for _, id := range []string{"a", "b"} {
		store.SetSynthesizedMeta(id, &ProjectMeta{Name: id})
		store.SetWorkspaceID(id, "team")
	}
	got, err := store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 2 || got.CardCommands["judge"].Run != "echo custom" || got.CardEvents.Command != "judge" {
		t.Fatalf("receiver: %+v", got)
	}
	for _, id := range []string{"a", "b"} {
		m, err := store.GetWithWorkspace(context.Background(), id)
		if err != nil || len(m.Triggers) != 0 || len(m.CardCommands) != 0 {
			t.Fatalf("ordinary %s: %+v %v", id, m, err)
		}
	}
	projectMeta, err := parseProjectMetaBytes("a", false, []byte(`name: A
triggers:
 - name: sweep
   every: 3m
   run: echo project sweep
signals:
 sources:
  - connector: demo/poll
    service: demo
    every: 4m
card_commands:
 judge:
  label: Project
  run: echo project
`))
	if err != nil {
		t.Fatal(err)
	}
	store.SetSynthesizedMeta("a", projectMeta)
	got, err = store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 0 || got.CardCommands["judge"].Run == "echo custom" || got.CardCommands["z-last"].Run != "echo last" {
		t.Fatalf("priority: %+v", got)
	}
	project, err := store.GetWithWorkspace(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Triggers) != 2 || project.Triggers[0].Run != "echo project sweep" || project.CardCommands["judge"].Run != "echo project" {
		t.Fatalf("project priority: %+v", project)
	}
	store.SetWorkspaceID("a", "other")
	got, err = store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 2 || got.CardCommands["judge"].Run != "echo custom" {
		t.Fatal("foreign workspace suppressed definitions")
	}

}

func TestWorkspaceAutomationValidation(t *testing.T) {
	for _, data := range []string{
		"triggers: [{name: bad, every: 1s, run: echo}]",
		"signals: {sources: [{connector: invalid, every: 1m}]}",
		"card_commands: {bad: {label: Bad}}",
		"card_events: {commnad: judge}",
	} {
		var m WorkspaceMeta
		if err := yaml.Unmarshal([]byte(data), &m); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
}

func TestWorkspaceAutomationEmptyPreservesMeta(t *testing.T) {
	ws := NewWorkspaceStore(t.TempDir())
	if err := ws.Save("team", &WorkspaceMeta{}); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(ws)
	for _, id := range []string{DefaultMetaprojectID("team"), "project"} {
		original := DefaultMetaprojectMeta("team")
		original.ID = id
		store.SetSynthesizedMeta(id, original)
		store.SetWorkspaceID(id, "team")
		got, err := store.GetWithWorkspace(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		want := cloneProjectMeta(original)
		want.SecretNamespace = "team"
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("empty workspace changed %s: got %+v want %+v", id, got, want)
		}
	}
}

func TestWorkspaceAutomationApplyPresenceAndClear(t *testing.T) {
	current := automationMeta(t)
	for _, body := range []string{"env: {A: B}", "triggers: []\nsignals: {}\ncard_commands: {}\ncard_events: {}"} {
		docs, err := DecodeWorkspaceEnvelopeDocuments([]byte("apiVersion: boid.dev/v1\nkind: Workspace\nmetadata: {name: team}\nspec:\n  " + strings.ReplaceAll(body, "\n", "\n  ")))
		if err != nil {
			t.Fatal(err)
		}
		got := docs[0].MergeInto(current)
		if strings.HasPrefix(body, "env") {
			if !reflect.DeepEqual(got.CardCommands, current.CardCommands) || !reflect.DeepEqual(got.Triggers, current.Triggers) {
				t.Fatal("absent fields changed")
			}
		} else if len(got.Triggers) != 0 || len(got.Signals.Sources) != 0 || len(got.CardCommands) != 0 || got.CardEvents.Command != "" {
			t.Fatalf("clear: %+v", got)
		}
	}
}

func TestWorkspaceAutomationStrictAndSaveValidation(t *testing.T) {
	got, err := DecodeWorkspaceMetaStrict([]byte(workspaceAutomationYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got.CardEvents.Command != "judge" || !reflect.DeepEqual(got.CardCommandsOrder, []string{"z-last", "judge"}) {
		t.Fatalf("strict: %+v", got)
	}
	repo := newTestWorkspaceRepo(t)
	bad := &WorkspaceMeta{Triggers: []Trigger{{Name: "bad", Run: "echo", Every: "1s"}}}
	if err := repo.Save("bad", bad); err == nil {
		t.Fatal("Save accepted invalid trigger")
	}
	if err := repo.Create("bad", bad); err == nil {
		t.Fatal("Create accepted invalid trigger")
	}
	good := automationMeta(t)
	if err := repo.Save("team", good); err != nil {
		t.Fatal(err)
	}
	_, rev, err := repo.LoadWithRevision("team")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpdateIfRevisionMatches("team", rev, bad); err == nil {
		t.Fatal("CAS accepted invalid trigger")
	}
}

func TestWorkspaceAutomationCardEventsInTransaction(t *testing.T) {
	repo := newTestWorkspaceRepo(t)
	meta := automationMeta(t)
	meta.CardEvents.Command = "z-last"
	if err := repo.Save("team", meta); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(NewWorkspaceStoreWithRepo(t.TempDir(), repo))
	if err := EnsureDefaultMetaprojects(repo.conn, store); err != nil {
		t.Fatal(err)
	}
	receiver := DefaultMetaprojectID("team")
	tx, err := repo.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO tasks (id,type,project_id,title,status,kind,urgency,wake_task_id,suggestion_verb,detail) VALUES ('card','card',?,'Card','parked','','','','','{}')`, receiver)
	if err != nil {
		t.Fatal(err)
	}
	if err := IngestCardEventRequest(context.Background(), tx, &Action{ID: "event", TaskID: "card", Type: ActionTypeCardEdited}, store); err != nil {
		t.Fatal(err)
	}
	requests, err := ListCardRequestsByCard(tx, "card")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].CommandKey != "z-last" {
		t.Fatalf("requests: %+v", requests)
	}
}

func TestWorkspaceAutomationShadowedEventKeepsBuiltinJudge(t *testing.T) {
	ws := NewWorkspaceStore(t.TempDir())
	meta := automationMeta(t)
	meta.CardEvents.Command = "z-last"
	meta.CardCommands["other"] = CardCommand{Label: "Other", Run: "echo other"}
	var warnings bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnings, nil)))
	defer slog.SetDefault(logger)
	if err := ws.Save("team", meta); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(ws)
	receiver := DefaultMetaprojectID("team")
	store.SetSynthesizedMeta(receiver, DefaultMetaprojectMeta("team"))
	store.SetWorkspaceID(receiver, "team")
	store.SetSynthesizedMeta("project", &ProjectMeta{CardCommands: map[string]CardCommand{"z-last": {Label: "Project", Run: "echo project"}}})
	store.SetWorkspaceID("project", "team")
	got, err := store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if got.CardEvents.Command != "judge" {
		t.Fatalf("event: %+v", got.CardEvents)
	}
	if _, ok := got.CardCommands["z-last"]; ok {
		t.Fatal("shadowed command remains")
	}
	if got.CardCommands["judge"] != DefaultMetaprojectMeta("team").CardCommands["judge"] {
		t.Fatal("built-in judge changed")
	}
	if got.CardCommands["other"].Run != "echo other" {
		t.Fatal("independent command was lost")
	}
	if !strings.Contains(warnings.String(), "workspace card events command shadowed") {
		t.Fatal("missing warning")
	}
	project, err := store.GetWithWorkspace(context.Background(), "project")
	if err != nil {
		t.Fatal(err)
	}
	if project.CardCommands["z-last"].Run != "echo project" {
		t.Fatal("project command changed")
	}
}

func TestWorkspaceAutomationCommandOrderOnReceiver(t *testing.T) {
	ws := NewWorkspaceStore(t.TempDir())
	if err := ws.Save("team", automationMeta(t)); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	store.SetWorkspaceStore(ws)
	receiver := DefaultMetaprojectID("team")
	store.SetSynthesizedMeta(receiver, DefaultMetaprojectMeta("team"))
	store.SetWorkspaceID(receiver, "team")
	got, err := store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.CardCommandsOrder, []string{"z-last", "judge"}) {
		t.Fatalf("order: %v", got.CardCommandsOrder)
	}
}

func TestWorkspaceAutomationMultilineValuesRoundTrip(t *testing.T) {
	repo := newTestWorkspaceRepo(t)
	for _, value := range []string{"\n\nhello\n", "\tline\n  next", "plain\nlast\n\n", "\x00"} {
		meta := automationMeta(t)
		cmd := meta.CardCommands["z-last"]
		cmd.Run = "echo " + value
		meta.CardCommands["z-last"] = cmd
		meta.Triggers[0].Run = "echo " + value
		meta.Signals.Sources[0].Config = map[string]any{"value": value}
		if err := repo.Save("team", meta); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Load("team")
		if err != nil {
			t.Fatal(err)
		}
		if got.CardCommands["z-last"].Run != cmd.Run || got.Triggers[0].Run != meta.Triggers[0].Run || !reflect.DeepEqual(got.Signals, meta.Signals) {
			t.Fatalf("DB lost %q", value)
		}
		data, err := MarshalWorkspaceEnvelope(NewWorkspaceEnvelopeFromMeta("team", got, nil, ""))
		if err != nil {
			t.Fatal(err)
		}
		docs, err := DecodeWorkspaceEnvelopeDocuments(data)
		if err != nil {
			t.Fatal(err)
		}
		restored := docs[0].MergeInto(nil)
		if restored.CardCommands["z-last"].Run != cmd.Run || restored.Triggers[0].Run != meta.Triggers[0].Run || !reflect.DeepEqual(restored.Signals, meta.Signals) {
			t.Fatalf("envelope lost %q", value)
		}
	}
}
