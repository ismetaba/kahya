package main

import (
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/ritual"
)

// TestRitualMaxQuestionsWiredFromConfig (FIX-MEM-5, decision MEM-5.b): the
// weekly ritual's sample size is config ritual_max_questions, set on the
// one sampler before the engine is built from it; the config default is the
// package's own cap.
func TestRitualMaxQuestionsWiredFromConfig(t *testing.T) {
	set := onlyCallSite(t, "ritualSampler", "SetMaxQuestions", "cfg.RitualMaxQuestions")
	engine := onlyCallSite(t, "ritual", "New", "")
	if set.offset > engine.offset {
		t.Fatal("ritualSampler.SetMaxQuestions must run before ritual.New takes the sampler")
	}
	if config.DefaultRitualMaxQuestions != ritual.MaxQuestionsPerRun {
		t.Fatalf("config.DefaultRitualMaxQuestions = %d, ritual.MaxQuestionsPerRun = %d; want equal", config.DefaultRitualMaxQuestions, ritual.MaxQuestionsPerRun)
	}
}
