# Auto-continue a parked task

**English** | [中文](continue.zh-CN.md)

Back to the [README](../README.md) · [Docs index](README.md).

`watch` and `bg` keep your window chain warm, but they don't resume a task that
has already stalled at the 5h limit. `limitping continue <provider>` does: it
launches the provider's real interactive CLI through a PTY and passes your
terminal straight through, so you drive Codex / Claude Code exactly as usual.
In the background it polls usage and, the moment the 5h limit recovers after
being hit, types your continue message into the session so a long task resumes
itself instead of sitting parked until you come back.

```sh
limitping continue codex                       # drive Codex as usual; auto-resume on recovery
limitping continue codex --yolo                # flags after the provider pass through verbatim
limitping continue claude --dangerously-skip-permissions
```

- The resume message is each provider's `continue_prompt` in config (default
  `"continue"`; set it to e.g. `"继续任务"`). Quit from inside the CLI to exit.
- It only injects on a genuine recovery edge: the 5h window was maxed (or the
  endpoint reported `limit_reached`, or the CLI printed a limit message) and has
  since clearly reset, and the weekly window isn't also exhausted (per
  `weekly_threshold`, credits included) — so it won't resume straight into the
  weekly wall.
- Recent Claude Code can resume a task itself once the limit resets ("Continue
  automatically at usage limit" in `/config`, on by default). `continue claude`
  defers to it: at recovery it waits up to 90 seconds for Claude Code to announce
  the resume or start the turn, and only types the message when it didn't — so
  the task is never continued twice, and older Claude Code still gets resumed.
- A diagnostic timeline is written to `~/.config/limitping/continue.log`
  (rotated to `continue.log.1` past 1 MB).
- Works on Unix (PTY) and Windows 10 1809+ (ConPTY). Windows notes:
  - Ctrl+Z is not forwarded to the CLI (the console reports it as end of input,
    which `continue` ignores so typing keeps working).
  - Closing the console window or pressing Ctrl+Break force-kills the CLI and
    its child processes at once, so it gets no chance to save session state;
    exit the CLI normally (e.g. `/exit`) when that matters. Ctrl+C reaches the
    CLI as a keystroke, as on Unix.
  - For an npm-installed `claude` (a `.cmd` shim) the process id used to detect
    that Claude Code resumed by itself is looked up on a best-effort basis; when
    it cannot be identified, `continue` relies on the on-screen message and the
    90-second grace only, which can occasionally type the message on top of a
    task Claude Code already resumed.
  - Your own typing is not interleaved with an injected message, but text you
    already typed into the prompt before the injection is not protected.
