package herdr

import "testing"

func TestAPIErrorReadsTheEnvelopeFromEitherStream(t *testing.T) {
	envelope := `{"error":{"code":"agent_prompt_stalled","message":"no state change"},"id":"cli:agent:prompt"}`

	// herdr writes the envelope to stderr for some subcommands and stdout for
	// others. Matching the formatted message instead of the parsed code hid
	// this: the raw JSON happened to contain the code as a substring.
	for name, err := range map[string]error{
		"stdout": apiError("agent prompt", []byte(envelope), ""),
		"stderr": apiError("agent prompt", nil, envelope),
	} {
		if !hasCode(err, codePromptStalled) {
			t.Errorf("%s: hasCode false for %v", name, err)
		}
	}

	// Plain text still surfaces, just without a code to branch on.
	plain := apiError("workspace list", nil, "herdr: not running")
	if hasCode(plain, codePromptStalled) {
		t.Error("plain stderr reported a code")
	}
	if plain.Error() != "workspace list: herdr: not running" {
		t.Errorf("plain error = %q", plain.Error())
	}
}
