package transcript

import "testing"

// Claude trims a caller-appended trailing newline from the prompt before
// storing it; the trimmed form must be among promptForms so Select matches.
func TestPromptFormsIncludesTrimmed(t *testing.T) {
	c := &Catalog{}
	forms := c.promptForms("<current_message from=\"E\">\nhi\n</current_message>\n\n")
	want := "<current_message from=\\\"E\\\">\\nhi\\n</current_message>" // non-HTML JSON body, trimmed
	found := false
	for _, f := range forms {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("trimmed non-HTML form %q not in forms %q", want, forms)
	}
}
