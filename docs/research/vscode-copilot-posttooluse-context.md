# VS Code Copilot Chat `PostToolUse` context and feedback

_Research date: 2026-08-30. Scope: command hooks in VS Code 1.135 / bundled Copilot Chat 0.63._

## Conclusion

**The documented contract supports both model-visible `PostToolUse` forms, but VS Code 1.135's panel command-hook path does not reliably deliver either form to the immediate next model request.** The 1.135 source accepts and aggregates nested `hookSpecificOutput.additionalContext` and top-level `decision: "block"` / `reason`; however, it starts the asynchronous post-hook/context append without awaiting it. The next prompt can therefore be assembled before either message reaches the tool result. An open upstream fix describes the same symptom and changes that call to `await`.

This reconciles the observation: “successful” in the hook log proves that the command ran and its JSON was parsed, not that the asynchronous context mutation completed before the next model request. A model's failure to repeat a marker is not by itself proof that it did not see it, but the 1.135 race makes absence from the actual next request payload expected. Verify delivery in the next LLM request input, not only in visible answer/reasoning text.

## Documented response shape

The current official [hooks reference, **PostToolUse output**](https://code.visualstudio.com/docs/agents/reference/hooks-reference#_posttooluse-output) documents this combined shape:

```json
{
  "decision": "block",
  "reason": "Post-processing validation failed",
  "hookSpecificOutput": {
    "hookEventName": "PostToolUse",
    "additionalContext": "The edited file has lint errors that need to be fixed"
  }
}
```

The same table says `reason` is shown to the model and `hookSpecificOutput.additionalContext` is injected into the conversation. `decision` has only the optional value `"block"`. The [official hooks guide](https://code.visualstudio.com/docs/copilot/customization/hooks#_hook-input-and-output) distinguishes common flow-control output from event-specific `hookSpecificOutput`.

The release source's command contract agrees: `PostToolUse` input adds `tool_name`, `tool_input`, `tool_response`, and `tool_use_id`, while its nested output has only optional `hookEventName` and `additionalContext` ([1.135 source, lines 38–56](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/platform/chat/common/hookCommandTypes.ts#L38-L56)). `permissionDecision`, `permissionDecisionReason`, and `updatedInput` are **PreToolUse**, not documented PostToolUse feedback fields ([lines 14–34](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/platform/chat/common/hookCommandTypes.ts#L14-L34)).

`decision: "block"` is post-execution feedback: the tool has already completed. In the implementation it becomes model-visible text saying the hook blocked the tool result; it does not undo the tool.

## What the 1.135 command-hook runtime implements

At the pinned `release/1.135` commit (Copilot extension package version 0.63.0):

1. Successful command output remains structured JSON after common fields are removed; nested `hookEventName` is validated and matching `hookSpecificOutput` is preserved ([parser, lines 261–335](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/extension/chat/vscode-node/chatHookService.ts#L261-L335)).
2. `executePostToolUseHook` collects nested `additionalContext`, recognizes top-level `decision === "block"`, retains `reason`, and returns both in a collapsed result ([lines 491–586](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/extension/chat/vscode-node/chatHookService.ts#L491-L586)). Thus both observed payloads are valid and a success log is unsurprising.
3. The intended model handoff appends block feedback and additional context to the tool result as `LanguageModelTextPart` values inside `<PostToolUse-context>` tags ([lines 630–649](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/extension/prompts/node/panel/toolCalling.tsx#L630-L649)).
4. The defect is one level above: `appendHookContext(...)` is called without `await` ([line 341](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/extension/prompts/node/panel/toolCalling.tsx#L334-L345)), although that helper is async and awaits command execution ([lines 600–649](https://github.com/microsoft/vscode/blob/08d4889f9ec4a1685d257b9b95de036c8e1ce1e5/extensions/copilot/src/extension/prompts/node/panel/toolCalling.tsx#L600-L649)). Prompt rendering can finish first. Both nested context and block/reason use this same late append, so both can disappear from the immediate model turn.

## Upstream corroboration

- [microsoft/vscode#314118](https://github.com/microsoft/vscode/issues/314118) reports successful `PostToolUse` execution whose feedback arrives too late.
- Open PR [microsoft/vscode#331785](https://github.com/microsoft/vscode/pull/331785), **“fix: await PostToolUse context in panel tool calls,”** identifies the unawaited helper as root cause, says the panel may assemble the next model request before context is appended, and reports reproduction on VS Code 1.134 / Copilot Chat 0.62. Its production fix is the missing `await`. The same unawaited line remains in the pinned 1.135 / 0.63 source above.
- A separate agent-host path previously had the same externally visible failure: [issue #311138](https://github.com/microsoft/vscode/issues/311138) records context in the transcript but not the same-turn request; merged [PR #311984](https://github.com/microsoft/vscode/pull/311984) fixed that path by returning parsed PostToolUse command output instead of discarding it. This explains why documentation and some runtime paths/tests can indicate support while another live path still fails.

## Practical answer

Treat the shapes as **valid documented outputs but not reliable immediate model feedback on VS Code 1.135 / Copilot Chat 0.63's panel path**. Neither swapping nested `additionalContext` for top-level `decision/reason` nor combining them avoids the race. Reliability requires the upstream await fix (or a build containing an equivalent change); after that, inspect the next model request payload for the marker to distinguish transport from a model choosing not to mention it.
