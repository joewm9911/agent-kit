package serving

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// Texts holds every end-user-facing string a channel emits, so a deployment
// can translate or reword them per binding without forking the framework.
// Defaults are English (see defaultTexts); a Chinese-facing IM bot overrides
// the fields it cares about. Fields whose comment shows % verbs are fmt
// templates — preserve the verbs and their order when overriding.
type Texts struct {
	Placeholder string // processing placeholder (no verbs)
	StepRunning string // in-progress step line: %s = step name
	StepDone    string // finished step line: %s name, %.1f seconds
	StepFailed  string // failed step line: %s name, %.1f seconds
	Summary     string // completion meta: %.1f seconds, %d tool calls
	Stopped     string // ack for a stop command (no verbs)
	Steered     string // ack for a steer command (no verbs)
	Overloaded  string // session queue full (no verbs)
	Thinking    string // stream placeholder (no verbs)
	Suspended   string // waiting-for-reply close line (no verbs)
	Failure     string // error close line: %s = error text
	Approval    string // approval question: %s description, %s arguments
	AskTimeout  string // ask/approval reply timeout (no verbs)
	Deliverable string // deliverable follow-up header: %s id, %s title; empty format = content only
}

// defaultTexts is the English baseline used when a Binding sets no Texts.
// The approval prompt is shared with the suspend package so the wording is
// identical whether the turn suspends or blocks in-process.
var defaultTexts = Texts{
	Placeholder: "⏳ Working…",
	StepRunning: "⚙ %s…",
	StepDone:    "✓ %s (%.1fs)",
	StepFailed:  "✗ %s (%.1fs) failed",
	Summary:     "%.1fs · %d tool calls",
	Stopped:     "OK, stopping the current task.",
	Steered:     "Passed your note to the running task.",
	Overloaded:  "Too many messages — please try again shortly.",
	Thinking:    "Thinking…",
	Suspended:   "⏸ I've asked you a question; I'll continue after your reply.",
	Failure:     "Failed: %s",
	Approval:    suspend.DefaultApprovalPrompt,
	AskTimeout:  "timed out waiting for user reply",
	Deliverable: "**%s · %s**\n\n", // 头部:%s=id 引用锚,%s=标题;配空串 = 只发原文
}

// NewTexts builds an override Texts from a map of snake_case field names
// (placeholder, step_running, step_done, step_failed, summary, stopped,
// steered, overloaded, thinking, suspended, failure, approval, ask_timeout).
// Unknown keys fail fast so a typo in YAML is caught at assembly. An empty
// or nil map returns nil (use the English default).
func NewTexts(overrides map[string]string) (*Texts, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	fields := map[string]*string{}
	var t Texts
	fields["placeholder"] = &t.Placeholder
	fields["step_running"] = &t.StepRunning
	fields["step_done"] = &t.StepDone
	fields["step_failed"] = &t.StepFailed
	fields["summary"] = &t.Summary
	fields["stopped"] = &t.Stopped
	fields["steered"] = &t.Steered
	fields["overloaded"] = &t.Overloaded
	fields["thinking"] = &t.Thinking
	fields["suspended"] = &t.Suspended
	fields["failure"] = &t.Failure
	fields["approval"] = &t.Approval
	fields["ask_timeout"] = &t.AskTimeout
	for k, v := range overrides {
		dst, ok := fields[k]
		if !ok {
			return nil, fmt.Errorf("channel texts: unknown key %q (valid keys: %s)", k, knownTextKeys(fields))
		}
		*dst = v
	}
	return &t, nil
}

func knownTextKeys(fields map[string]*string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// texts returns the binding's Texts, falling back to the English default.
// A partially-filled Texts is used as-is with empty fields taken from the
// default, so an override only needs the strings it changes.
func (b Binding) texts() Texts {
	if b.Texts == nil {
		return defaultTexts
	}
	return b.Texts.fill()
}

// fill returns t with any empty field taken from the English default.
func (t Texts) fill() Texts {
	d := defaultTexts
	set := func(dst *string, def string) {
		if *dst == "" {
			*dst = def
		}
	}
	set(&t.Placeholder, d.Placeholder)
	set(&t.StepRunning, d.StepRunning)
	set(&t.StepDone, d.StepDone)
	set(&t.StepFailed, d.StepFailed)
	set(&t.Summary, d.Summary)
	set(&t.Stopped, d.Stopped)
	set(&t.Steered, d.Steered)
	set(&t.Overloaded, d.Overloaded)
	set(&t.Thinking, d.Thinking)
	set(&t.Suspended, d.Suspended)
	set(&t.Failure, d.Failure)
	set(&t.Approval, d.Approval)
	set(&t.AskTimeout, d.AskTimeout)
	return t
}
