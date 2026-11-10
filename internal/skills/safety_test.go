package skills

import (
	"regexp"
	"strings"
	"testing"
)

// safetyRules are the safety rules of design 7.9 (from the retro
// aggregate's P0 and P1 findings), each with the phrases that state it.
// Every repository skill states every rule; the W6 drafts of the
// browser-checker prompt and the safari-agent skill are checked for the
// same rules by proposals/w6/check_w6.py.
var safetyRules = []struct {
	rule    string
	phrases []string
}{
	{"no infrastructure writes, stop on a guard refusal", []string{"No infrastructure writes", "guard or wrapper refuses", "stop and report the refusal"}},
	{"build, origin and auth preflight; one final verdict", []string{"Preflight", "build under test is live", "sign-in state", "VERDICT: PASS", "VERDICT: FAIL", "VERDICT: BLOCKED"}},
	{"one bounded diagnostic budget, then BLOCKED", []string{"One bounded diagnostic budget, then BLOCKED", "fails twice", "health probe"}},
	{"masking, 0600, HAR and state deletion, no account page dumps", []string{"password and token fields", "[masked]", "mode 0600", "delete them before the run ends", "account, security or settings pages"}},
	{"preserve and restore cookie owners; revocation by the auth helper", []string{"Preserve and restore cookie owners", "restore it", "belongs to the sign-in helper"}},
	{"bounded shell calls, known evidence folders, no filesystem-wide search", []string{"Give every shell call a timeout", "evidence folder", "Never search the filesystem"}},
	{"never stop or restart another agent's iwdp; reserve and release", []string{"Never stop, restart or kill an iwdp", "Reserve the device", "release it at the end"}},
}

func TestEverySkillStatesTheSafetyRules(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		for _, r := range safetyRules {
			for _, p := range r.phrases {
				if !strings.Contains(s.Content, p) {
					t.Errorf("skill %s: safety rule %q: missing %q", s.Name, r.rule, p)
				}
			}
		}
	}
}

// rawOutputs are the outputs that print page data unmasked
// (internal/runtime/js/45-locate.js returns a field's raw value;
// internal/commands/console.go and errors.go format raw messages and
// stacks; eval returns what the page returns), named by the skill that
// documents them. Each skill must say that they are not masked.
var rawOutputs = map[string][]string{
	"iwdp": {"get value", "get text", "`eval`", "`console`", "`errors`", "snapshot text"},
}

// rawTerms are words that name a raw output in any skill.
var rawTerms = []string{"console", "errors", "get value", "get text", "`eval`", "snapshot"}

// sentences splits a skill into sentences and clauses.
var sentenceEnd = regexp.MustCompile(`[.;:]\s|\n`)

// Review W6-b F1: masking happens only in cookies, the network log, HAR
// files and storage. A skill names the raw outputs in one "Page reads are
// not masked" sentence, and no sentence that promises masking names a raw
// output.
func TestSkillsLimitMaskingToTheMaskedOutputs(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		want, ok := rawOutputs[s.Name]
		if !ok {
			t.Errorf("skill %s: no raw-output list in rawOutputs", s.Name)
			continue
		}
		_, after, found := strings.Cut(s.Content, "Page reads are not masked")
		raw, _, _ := strings.Cut(after, ".")
		if !found {
			t.Errorf("skill %s: no sentence \"Page reads are not masked: ...\"", s.Name)
		}
		for _, w := range want {
			if found && !strings.Contains(raw, w) {
				t.Errorf("skill %s: the raw-output sentence does not name %s", s.Name, w)
			}
		}
		for _, sent := range sentenceEnd.Split(s.Content, -1) {
			l := strings.ToLower(sent)
			claims := strings.Contains(l, "masked") || strings.Contains(l, "redact")
			if !claims || strings.Contains(l, "not masked") || strings.Contains(l, "unmasked") {
				continue
			}
			for _, term := range rawTerms {
				if strings.Contains(l, term) {
					t.Errorf("skill %s: a masking claim names the raw output %s: %q", s.Name, term, sent)
				}
			}
		}
	}
}
