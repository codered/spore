package kernel

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

const yahooBody = `{"chart":{"result":[{"meta":{"currency":"USD","regularMarketPrice":212.5},` +
	`"timestamp":[1,2,3],"indicators":{"quote":[{"close":[200.1,205.2,212.5],"open":[1,2,3]}]}}],"error":null}}`

func TestJSONGet(t *testing.T) {
	cases := map[string]struct {
		path string
		want any
	}{
		"nested":     {"chart.result.0.meta.currency", "USD"},
		"number":     {"chart.result.0.meta.regularMarketPrice", 212.5},
		"last":       {"chart.result.0.indicators.quote.0.close.-1", 212.5},
		"brackets":   {"chart.result[0].indicators.quote[0].close[1]", 205.2},
		"dollar":     {"$.chart.error", nil},
		"whole body": {"", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := jsonGet(yahooBody, c.path)
			if err != nil {
				t.Fatalf("jsonGet(%q): %v", c.path, err)
			}
			if c.path == "" {
				if _, ok := got.(map[string]any); !ok {
					t.Errorf("empty path = %T, want the whole object", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("jsonGet(%q) = %v (%T), want %v", c.path, got, got, c.want)
			}
		})
	}
}

// The point of JSONGet over a guessed struct: a wrong guess fails loudly
// and says what is actually there.
func TestJSONGetErrorsSayWhatIsThere(t *testing.T) {
	cases := map[string]struct{ path, want string }{
		"missing key":  {"chart.result.0.indicators.close", `no key "close" at chart.result[0].indicators; keys there: quote`},
		"dotted guess": {"chart.result.0.indicatorz", "keys there: meta, timestamp, indicators"},
		"out of range": {"chart.result.3", "index 3 out of range at chart.result (length 1)"},
		"not an array": {"chart.result.0.meta.0", "chart.result[0].meta is an object, not an array"},
		"into scalar":  {"chart.result.0.meta.currency.x", `chart.result[0].meta.currency is a string ("USD"), not an object`},
		"into null":    {"chart.error.code", "chart.error is null, not an object"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := jsonGet(yahooBody, c.path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestJSONGetOnABodyThatIsNotJSON(t *testing.T) {
	_, err := jsonGet("<html><body>Too Many Requests</body></html>", "a")
	if err == nil || !strings.Contains(err.Error(), "not JSON") || !strings.Contains(err.Error(), "Too Many Requests") {
		t.Errorf("err = %v, want it to say not JSON and show the start of the body", err)
	}
}

func TestJSONShape(t *testing.T) {
	got := jsonShape(yahooBody)
	for _, want := range []string{
		"chart: {",
		"result: [1 × {",
		`currency: string "USD"`,
		"regularMarketPrice: number 212.5",
		"timestamp: [3 × number]",
		"close: [3 × number]",
		"error: null",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("shape lacks %q:\n%s", want, got)
		}
	}
	// Keys keep the body's order, so the outline reads like the document.
	if strings.Index(got, "meta:") > strings.Index(got, "indicators:") {
		t.Errorf("keys are out of document order:\n%s", got)
	}
}

func TestJSONShapeIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"k` + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + `":"` + strings.Repeat("long value ", 20) + `"`)
	}
	b.WriteString("}")
	got := jsonShape(b.String())
	if len(got) > 4000 {
		t.Errorf("shape of a wide object is %d bytes; it must stay small", len(got))
	}
	if !strings.Contains(got, "more keys") {
		t.Errorf("a cut object does not say so:\n%s", got)
	}
	deep := strings.Repeat(`{"a":`, 40) + "1" + strings.Repeat("}", 40)
	if got := jsonShape(deep); !strings.Contains(got, "…") {
		t.Errorf("a deep document is not cut:\n%s", got)
	}
	if got := jsonShape("nope"); !strings.Contains(got, "not JSON") {
		t.Errorf("shape of a non-JSON body = %q", got)
	}
}

func TestJSONHelpersInAProgram(t *testing.T) {
	r := &fakeRunner{}
	src := prog(`func main() {
	body, _ := spore.Fetch("https://x.test")
	fmt.Print(spore.JSONShape(body))
	v, err := spore.JSONGet(body, "chart.result.0.indicators.quote.0.close.-1")
	fmt.Println("last:", v, err)
	_, err = spore.JSONGet(body, "chart.result.0.indicators.close")
	fmt.Println("err:", err)
}`, "fmt", "spore")
	r.fn = func(_ context.Context, call provider.Block) provider.Block {
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: yahooBody}
	}
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"result: [1 × {", "last: 212.5 <nil>", `err: no key "close"`} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
	if n := len(r.seen()); n != 1 {
		t.Errorf("%d tool calls; the JSON helpers must answer locally", n)
	}
}
