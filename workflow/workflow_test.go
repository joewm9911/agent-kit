package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/cloverzhang/agent-kit/capability"
	"github.com/cloverzhang/agent-kit/internal/testmodel"
	"github.com/cloverzhang/agent-kit/source"
)

func TestWorkflowSequentialGraph(t *testing.T) {
	catalog := source.NewCatalog(capability.RiskMutating, nil)
	upper := capability.New(capability.Meta{
		Ref: capability.Ref{Kind: "tool", Provider: "test", Namespace: "t", Name: "upper"},
	}, func(ctx context.Context, in string) (string, error) {
		return strings.ToUpper(in), nil
	})
	if err := catalog.Add(upper); err != nil {
		t.Fatal(err)
	}
	m := testmodel.New(schema.AssistantMessage("summary-of-input", nil))

	wf, err := Build(context.Background(), Config{
		Name:        "demo",
		Description: "upper → model",
		Steps: []Step{
			{Name: "up", Use: "cap://tool.test/t/upper", Args: "{input}"},
			{Name: "sum", Use: "model", Args: "总结:{up}"},
		},
	}, catalog, m)
	if err != nil {
		t.Fatal(err)
	}

	if wf.Meta().Ref.String() != "cap://flow.workflow/workflows/demo" {
		t.Fatalf("ref = %s", wf.Meta().Ref)
	}
	out, err := capability.Invoke(context.Background(), wf, `{"input":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "summary-of-input" {
		t.Fatalf("got %q", out)
	}
}

func TestWorkflowUnknownCapability(t *testing.T) {
	catalog := source.NewCatalog(capability.RiskMutating, nil)
	_, err := Build(context.Background(), Config{
		Name:  "bad",
		Steps: []Step{{Name: "s", Use: "cap://tool.test/t/none"}},
	}, catalog, nil)
	if err == nil {
		t.Fatal("expect error for unresolved step capability")
	}
}
