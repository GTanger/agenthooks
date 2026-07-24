package agenthooks

import "fmt"

// Decisions carry *intent*; the provider codec translates intent into that
// provider's mechanism (JSON dialect, exit code, thrown error). Consumers
// never see the wire.

// DecisionKind identifies the outcome a decision carries. The values are
// plain ints with append-only ordering: new kinds are only ever added at the
// end, so persisted values never change meaning.
type DecisionKind int

const (
	// DecisionNoDecision defers to the provider's normal flow (NEVER a
	// forced allow). It is the zero value: a zero-value decision is neutral.
	DecisionNoDecision DecisionKind = iota
	DecisionAllow
	DecisionDeny
	DecisionAsk
	DecisionAcceptPrompt
	DecisionBlockPrompt
	DecisionFinish
	DecisionContinue
	DecisionObserved
	DecisionFlagOutput
	DecisionReplaceOutput
	DecisionContinueSession
)

// String returns a stable lower-case name for logs.
func (k DecisionKind) String() string {
	switch k {
	case DecisionNoDecision:
		return "no-decision"
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionAsk:
		return "ask"
	case DecisionAcceptPrompt:
		return "accept-prompt"
	case DecisionBlockPrompt:
		return "block-prompt"
	case DecisionFinish:
		return "finish"
	case DecisionContinue:
		return "continue"
	case DecisionObserved:
		return "observed"
	case DecisionFlagOutput:
		return "flag-output"
	case DecisionReplaceOutput:
		return "replace-output"
	case DecisionContinueSession:
		return "continue-session"
	}
	return fmt.Sprintf("decision-kind(%d)", int(k))
}

// Decision is the read-only view every decision type satisfies — the common
// surface Runner.Decide and middleware Next return. Consumers type-assert to
// the concrete decision type (ToolPreDecision, StopDecision, ...) when they
// need kind-specific fields such as Instruction or UpdatedInput. The
// interface is sealed: only decision values built by this package implement
// it, so every Decision can be carried back through the pipeline losslessly.
type Decision interface {
	Kind() DecisionKind
	Reason() string
	SystemMessage() string
	Context() []string

	// Blocks reports whether the decision prevents the gated action: true
	// exactly for the kinds whose intent is "the action is prevented" —
	// DecisionDeny and DecisionBlockPrompt today. An ask is not blocking
	// (it defers to a human), and every observe/continue kind is false.
	// Consumers branch on this instead of enumerating kinds; a blocking
	// kind added later will return true here without call-site changes.
	Blocks() bool

	decCore() decisionCore
}

type decisionCore struct {
	kind              DecisionKind
	reason            string
	instruction       string // ContinueWith payload
	context           []string
	systemMessage     string
	stopAgent         bool
	stopReason        string
	updatedInput      any
	hasUpdatedInput   bool
	replacedOutput    any
	hasReplacedOutput bool
}

// blocks is the single source of truth for Decision.Blocks: only the kinds
// whose intent is "the action is prevented" are blocking.
func (c decisionCore) blocks() bool {
	switch c.kind {
	case DecisionDeny, DecisionBlockPrompt:
		return true
	default:
		return false
	}
}

func (c decisionCore) withContext(s string) decisionCore {
	c.context = append(append([]string(nil), c.context...), s)
	return c
}

// contextCopy returns the context strings without aliasing the internal
// slice, so read accessors can't be used to mutate a decision.
func (c decisionCore) contextCopy() []string {
	if len(c.context) == 0 {
		return nil
	}
	return append([]string(nil), c.context...)
}

// ToolPreDecision gates tool.pre and permission.request events.
type ToolPreDecision struct{ core decisionCore }

// NoDecision defers to the provider's normal permission flow. It is NEVER a
// forced allow: the codecs emit each provider's correct "no opinion" form.
func NoDecision() ToolPreDecision { return ToolPreDecision{decisionCore{kind: DecisionNoDecision}} }

// Allow skips the permission prompt where supported. It never loosens policy:
// provider-side deny rules still apply.
func Allow() ToolPreDecision { return ToolPreDecision{decisionCore{kind: DecisionAllow}} }

// Deny blocks the tool call with feedback for the model.
func Deny(reason string) ToolPreDecision {
	return ToolPreDecision{decisionCore{kind: DecisionDeny, reason: reason}}
}

// AskUser forces a confirmation prompt where the provider supports one.
// Behavior on providers without ask is governed by Policy.AskFallback.
func AskUser(reason string) ToolPreDecision {
	return ToolPreDecision{decisionCore{kind: DecisionAsk, reason: reason}}
}

// WithUpdatedInput rewrites the tool arguments before execution.
func (d ToolPreDecision) WithUpdatedInput(v any) ToolPreDecision {
	d.core.updatedInput = v
	d.core.hasUpdatedInput = true
	return d
}

// WithContext injects context for the model where supported.
func (d ToolPreDecision) WithContext(s string) ToolPreDecision {
	d.core = d.core.withContext(s)
	return d
}

// WithSystemMessage attaches a user-facing note where supported.
func (d ToolPreDecision) WithSystemMessage(s string) ToolPreDecision {
	d.core.systemMessage = s
	return d
}

// StopAgent requests continue:false where supported.
func (d ToolPreDecision) StopAgent(reason string) ToolPreDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// Kind reports the decision outcome.
func (d ToolPreDecision) Kind() DecisionKind { return d.core.kind }

// Reason reports the reason the decision was constructed with.
func (d ToolPreDecision) Reason() string { return d.core.reason }

// SystemMessage reports the note attached with WithSystemMessage.
func (d ToolPreDecision) SystemMessage() string { return d.core.systemMessage }

// Context reports the context strings attached with WithContext.
func (d ToolPreDecision) Context() []string { return d.core.contextCopy() }

// Blocks reports whether the decision prevents the gated action.
func (d ToolPreDecision) Blocks() bool { return d.core.blocks() }

// UpdatedInput reports the rewritten tool arguments and whether a rewrite
// was attached with WithUpdatedInput.
func (d ToolPreDecision) UpdatedInput() (any, bool) {
	return d.core.updatedInput, d.core.hasUpdatedInput
}

func (d ToolPreDecision) decCore() decisionCore { return d.core }

// PromptDecision gates prompt.submitted events.
type PromptDecision struct{ core decisionCore }

func AcceptPrompt() PromptDecision {
	return PromptDecision{decisionCore{kind: DecisionAcceptPrompt}}
}

func BlockPrompt(reason string) PromptDecision {
	return PromptDecision{decisionCore{kind: DecisionBlockPrompt, reason: reason}}
}

func (d PromptDecision) WithContext(s string) PromptDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d PromptDecision) WithSystemMessage(s string) PromptDecision {
	d.core.systemMessage = s
	return d
}

func (d PromptDecision) StopAgent(reason string) PromptDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// Kind reports the decision outcome.
func (d PromptDecision) Kind() DecisionKind { return d.core.kind }

// Reason reports the reason the decision was constructed with.
func (d PromptDecision) Reason() string { return d.core.reason }

// SystemMessage reports the note attached with WithSystemMessage.
func (d PromptDecision) SystemMessage() string { return d.core.systemMessage }

// Context reports the context strings attached with WithContext.
func (d PromptDecision) Context() []string { return d.core.contextCopy() }

// Blocks reports whether the decision prevents the gated action.
func (d PromptDecision) Blocks() bool { return d.core.blocks() }

func (d PromptDecision) decCore() decisionCore { return d.core }

// StopDecision responds to agent.stop / subagent.stop.
type StopDecision struct{ core decisionCore }

// Finish lets the agent stop.
func Finish() StopDecision { return StopDecision{decisionCore{kind: DecisionFinish}} }

// ContinueWith keeps the agent working: claude decision:block+reason, cursor
// followup_message, codex continuation prompt, gemini retry. The runner
// enforces Policy.ContinuationCap so consumers can't build infinite loops on
// providers without native caps.
func ContinueWith(instruction string) StopDecision {
	return StopDecision{decisionCore{kind: DecisionContinue, instruction: instruction}}
}

func (d StopDecision) WithSystemMessage(s string) StopDecision {
	d.core.systemMessage = s
	return d
}

// Kind reports the decision outcome.
func (d StopDecision) Kind() DecisionKind { return d.core.kind }

// Reason reports the reason the decision was constructed with.
func (d StopDecision) Reason() string { return d.core.reason }

// SystemMessage reports the note attached with WithSystemMessage.
func (d StopDecision) SystemMessage() string { return d.core.systemMessage }

// Context reports the context strings attached to the decision.
func (d StopDecision) Context() []string { return d.core.contextCopy() }

// Blocks reports whether the decision prevents the gated action.
func (d StopDecision) Blocks() bool { return d.core.blocks() }

// Instruction reports the ContinueWith payload.
func (d StopDecision) Instruction() string { return d.core.instruction }

func (d StopDecision) decCore() decisionCore { return d.core }

// ToolPostDecision responds to tool.post / tool.error.
type ToolPostDecision struct{ core decisionCore }

// Observed acknowledges the event with no opinion.
func Observed() ToolPostDecision { return ToolPostDecision{decisionCore{kind: DecisionObserved}} }

// FlagOutput sends feedback about the tool output to the model.
func FlagOutput(reason string) ToolPostDecision {
	return ToolPostDecision{decisionCore{kind: DecisionFlagOutput, reason: reason}}
}

// ReplaceOutput substitutes the tool output where supported: claude
// updatedToolOutput, cursor updated_mcp_tool_output (MCP only), gemini
// tool_output, opencode output mutation.
func ReplaceOutput(v any) ToolPostDecision {
	return ToolPostDecision{decisionCore{
		kind:              DecisionReplaceOutput,
		replacedOutput:    v,
		hasReplacedOutput: true,
	}}
}

func (d ToolPostDecision) WithContext(s string) ToolPostDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d ToolPostDecision) WithSystemMessage(s string) ToolPostDecision {
	d.core.systemMessage = s
	return d
}

func (d ToolPostDecision) StopAgent(reason string) ToolPostDecision {
	d.core.stopAgent = true
	d.core.stopReason = reason
	return d
}

// Kind reports the decision outcome.
func (d ToolPostDecision) Kind() DecisionKind { return d.core.kind }

// Reason reports the reason the decision was constructed with.
func (d ToolPostDecision) Reason() string { return d.core.reason }

// SystemMessage reports the note attached with WithSystemMessage.
func (d ToolPostDecision) SystemMessage() string { return d.core.systemMessage }

// Context reports the context strings attached with WithContext.
func (d ToolPostDecision) Context() []string { return d.core.contextCopy() }

// Blocks reports whether the decision prevents the gated action.
func (d ToolPostDecision) Blocks() bool { return d.core.blocks() }

// ReplacedOutput reports the substituted tool output and whether the
// decision was constructed with ReplaceOutput.
func (d ToolPostDecision) ReplacedOutput() (any, bool) {
	return d.core.replacedOutput, d.core.hasReplacedOutput
}

func (d ToolPostDecision) decCore() decisionCore { return d.core }

// SessionStartDecision responds to session.start.
type SessionStartDecision struct{ core decisionCore }

func ContinueSession() SessionStartDecision {
	return SessionStartDecision{decisionCore{kind: DecisionContinueSession}}
}

func (d SessionStartDecision) WithContext(s string) SessionStartDecision {
	d.core = d.core.withContext(s)
	return d
}

func (d SessionStartDecision) WithSystemMessage(s string) SessionStartDecision {
	d.core.systemMessage = s
	return d
}

// Kind reports the decision outcome.
func (d SessionStartDecision) Kind() DecisionKind { return d.core.kind }

// Reason reports the reason the decision was constructed with.
func (d SessionStartDecision) Reason() string { return d.core.reason }

// SystemMessage reports the note attached with WithSystemMessage.
func (d SessionStartDecision) SystemMessage() string { return d.core.systemMessage }

// Context reports the context strings attached with WithContext.
func (d SessionStartDecision) Context() []string { return d.core.contextCopy() }

// Blocks reports whether the decision prevents the gated action.
func (d SessionStartDecision) Blocks() bool { return d.core.blocks() }

func (d SessionStartDecision) decCore() decisionCore { return d.core }
