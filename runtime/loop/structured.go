package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/joewm9911/agent-kit/runtime/engine"
)

// StructuredConfig 要求最终回答符合 JSON Schema(下游是程序时的刚需)。
type StructuredConfig struct {
	Schema     string `yaml:"schema" json:"schema"` // JSON Schema 文本
	MaxRetries int    `yaml:"max_retries" json:"max_retries"`
}

// StructuredEnforcer 校验并修复模型输出:不符合 schema 时把校验错误
// 回喂模型重试,直到合规或重试耗尽。校验与重试由代码保证,模型只负责改。
type StructuredEnforcer struct {
	schema     *jsonschema.Schema
	schemaText string
	maxRetries int
}

// NewStructuredEnforcer 编译 schema,配置为空时返回 nil(不启用)。
func NewStructuredEnforcer(cfg StructuredConfig) (*StructuredEnforcer, error) {
	if strings.TrimSpace(cfg.Schema) == "" {
		return nil, nil
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("output.json", strings.NewReader(cfg.Schema)); err != nil {
		return nil, fmt.Errorf("structured output: bad schema: %w", err)
	}
	s, err := compiler.Compile("output.json")
	if err != nil {
		return nil, fmt.Errorf("structured output: compile schema: %w", err)
	}
	retries := cfg.MaxRetries
	if retries <= 0 {
		retries = 2
	}
	return &StructuredEnforcer{schema: s, schemaText: cfg.Schema, maxRetries: retries}, nil
}

// Enforce 校验 answer;不合规时用 m 修复,返回合规的 JSON 文本。
func (e *StructuredEnforcer) Enforce(ctx context.Context, m model.ToolCallingChatModel, answer string) (string, error) {
	current := answer
	var lastErr error
	for i := 0; i <= e.maxRetries; i++ {
		candidate := extractJSONBlock(engine.ExtractJSON(current))
		if err := e.validate(candidate); err == nil {
			return candidate, nil
		} else {
			lastErr = err
		}
		if i == e.maxRetries {
			break
		}
		out, err := m.Generate(ctx, []*schema.Message{
			schema.SystemMessage("You must output only a single JSON object conforming to the following JSON Schema, and nothing else:\n" + e.schemaText),
			schema.UserMessage(fmt.Sprintf("Fix the following output to conform to the schema.\nValidation error: %v\nOriginal output:\n%s", lastErr, current)),
		})
		if err != nil {
			return "", err
		}
		current = out.Content
	}
	return "", fmt.Errorf("structured output: still invalid after %d retries: %w", e.maxRetries, lastErr)
}

func (e *StructuredEnforcer) validate(candidate string) error {
	var v any
	if err := json.Unmarshal([]byte(candidate), &v); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	return e.schema.Validate(v)
}

func extractJSONBlock(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	end := strings.LastIndexAny(s, "}]")
	if end <= start {
		return s
	}
	return s[start : end+1]
}
