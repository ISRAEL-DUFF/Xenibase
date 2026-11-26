package provision

import (
	"context"
	"encoding/json"
	"testing"
)

// A create's extra secrets (an import's sealed source URL) join the
// password handoff in params.secrets, which goes when the operation ends,
// and the password still opens.
func TestCreateKeepsExtraSecretsWithThePassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c, err := h.s.Create(ctx, CreateParams{OrgID: h.org, Name: "imported", Kind: "import",
		Params: map[string]any{"import": map[string]any{"schemas": []string{"public"}}}, Secrets: map[string]string{"source_url": "sealed"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.dropObjects(c.Project.DbName) })
	var params struct {
		Import  map[string]any    `json:"import"`
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(c.Operation.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Secrets["source_url"] != "sealed" || params.Secrets["password"] == "" || params.Import == nil {
		t.Fatalf("params: %s", c.Operation.Params)
	}
	if pw, err := h.s.Password(c.Operation); err != nil || pw != c.Password {
		t.Fatalf("password handoff: %v", err)
	}
}
