package main

import (
	"encoding/json"
	"os"
	"testing"
)

// Every manifest setting (and every choice option) must carry a label and a
// help line that resolve in locales/en.json: the till renders them through T,
// and shows the raw setting key only when they are missing (ut-docs#6570).
func TestEverySettingHasResolvingLabelAndHelp(t *testing.T) {
	var m struct {
		Settings []struct {
			Key     string `json:"key"`
			Label   string `json:"label"`
			Help    string `json:"help"`
			Options []struct {
				Value string `json:"value"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"settings"`
	}
	readJSON(t, "../manifest.json", &m)
	var en map[string]string
	readJSON(t, "../locales/en.json", &en)
	if len(m.Settings) == 0 {
		t.Fatal("manifest has no settings")
	}
	resolve := func(what, key string) {
		if key == "" {
			t.Errorf("%s: missing", what)
			return
		}
		if en[key] == "" {
			t.Errorf("%s: key %q has no text in locales/en.json", what, key)
		}
	}
	for _, s := range m.Settings {
		resolve(s.Key+" label", s.Label)
		resolve(s.Key+" help", s.Help)
		for _, o := range s.Options {
			resolve(s.Key+" option "+o.Value, o.Label)
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}
