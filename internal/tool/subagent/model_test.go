package subagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBothLaunchToolsOfferAModelArgument(t *testing.T) {
	for _, tl := range New(nil)[:2] {
		var schema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
			t.Fatal(err)
		}
		m, ok := schema.Properties["model"]
		if !ok || m.Type != "string" || !strings.Contains(m.Description, "/model") {
			t.Fatalf("%s: model property = %+v", tl.Name(), m)
		}
		for _, r := range schema.Required {
			if r == "model" {
				t.Fatalf("%s: model must be optional", tl.Name())
			}
		}
	}
}
