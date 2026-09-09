package ply

import (
	"testing"
)

func TestModesAreTurnScopedAndSurviveCompaction(t *testing.T) {
	mode := message("developer", "planning only")
	mode["ply.mode"] = "plan"
	items := []Item{mode, message("user", "old"), Item{"type": "ply.compaction", "summary": "summary"}, message("user", "new")}
	a := App{T: &Transcript{Items: items}, Modes: []Item{mode}}
	input := a.modelInput()
	count := 0
	for _, i := range input {
		if textOf(i) == "planning only" {
			count++
			if i["ply.mode"] != nil {
				t.Fatal(i)
			}
		}
	}
	if count != 1 {
		t.Fatal(input)
	}
	a.Modes = nil
	for _, i := range a.modelInput() {
		if textOf(i) == "planning only" {
			t.Fatal("old mode leaked")
		}
	}
}
