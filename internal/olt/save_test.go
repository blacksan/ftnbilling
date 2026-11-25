package olt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/olt/fake"
)

// The bench V1600GS-R's `write` outlasted the 20 s per-command timeout: the save has its own, longer one, and the common
// save outputs and its own confirmation question are accepted.
func TestSaveSlowAndChatty(t *testing.T) {
	for name, set := range map[string]func(f *fake.OLT){
		"slow":     func(f *fake.OLT) { f.WriteDelay = 600 * time.Millisecond },
		"building": func(f *fake.OLT) { f.WriteBuilding, f.WriteDelay = true, 600*time.Millisecond },
		"confirm": func(f *fake.OLT) {
			f.WriteAsk, f.WriteBuilding, f.WriteDelay = "Are you sure to save the configuration? [y/N]: ", true, 300*time.Millisecond
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			set(f)
			s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.Timeout = 200 * time.Millisecond // shorter than the save: only the save's own timeout lets it finish
			s.SaveTimeout = 5 * time.Second
			if err := s.Save(ctx(t)); err != nil {
				t.Fatal(err)
			}
			if f.Writes() != 1 {
				t.Fatalf("writes: %d", f.Writes())
			}
			// The session is usable afterwards, at the prompt.
			if _, err := s.Show(ctx(t), "show running-config"); err != nil && !strings.Contains(err.Error(), "refused") {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveAnswersNoToAnOtherQuestion(t *testing.T) {
	f := newFake(t)
	f.WriteAsk = "Reboot the system now? [y/N]: "
	s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Save(ctx(t))
	var ce *CmdError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "not the save confirmation") {
		t.Fatalf("got %v", err)
	}
	if f.Writes() != 0 {
		t.Fatal("saved after a question that is not the save confirmation")
	}
	if ev := strings.Join(f.Events(), "\n"); strings.Contains(ev, "cmd y") {
		t.Fatalf("answered y:\n%s", ev)
	}
}

func TestSaveTimesOutOnItsOwnBudget(t *testing.T) {
	f := newFake(t)
	f.WriteDelay = 2 * time.Second
	s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Timeout, s.SaveTimeout = 100*time.Millisecond, 300*time.Millisecond
	if err := s.Save(ctx(t)); !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "save did not finish") {
		t.Fatalf("got %v", err)
	}
}

func TestSaveConfirmShapes(t *testing.T) {
	d := VSOL()
	for _, q := range []string{"Are you sure to save the configuration? [y/N]: ", "Save current configuration? (y/n)", "Overwrite the startup configuration? [Y/N]:", "write configuration to flash? [yes/no]"} {
		if !d.QuestionRE.MatchString(strings.TrimSpace(q)) || !d.SaveConfirmRE.MatchString(strings.TrimSpace(q)) {
			t.Errorf("not taken as the save confirmation: %q", q)
		}
	}
	for _, q := range []string{"Reboot the system now? [y/N]:", "Erase the startup configuration? [y/N]", "Are you sure to save the configuration and reboot? [y/N]"} {
		if !d.QuestionRE.MatchString(q) || d.SaveConfirmRE.MatchString(q) {
			t.Errorf("wrongly taken as the save confirmation (or not a question): %q", q)
		}
	}
}
