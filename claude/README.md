# Connecting Azor to Claude Code

Skill (Claude queries history when it is relevant):

    azor install-skill

It writes the skill built into the binary to `~/.claude/skills/azor/SKILL.md` (or under `$CLAUDE_CONFIG_DIR`), replaces a link left by an older setup, and never overwrites a `SKILL.md` that is not Azor's. `make install` runs it, so the skill matches the installed version; after a package update, run it again. It also prints the hook below with the path of the binary.

Hook (one line of current state when a session starts). Merge into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "matcher": "startup|resume",
        "hooks": [ { "type": "command", "command": "\"$HOME/.local/bin/azor\" brief 2>/dev/null || true", "timeout": 3 } ] }
    ]
  }
}
```

The hook prints at most 600 characters and nothing when the daemon is off. Whatever it prints becomes part of the conversation sent to Anthropic.

Security of the hook:

- Use the full path to the binary, as above, not a name found through `PATH`; for the system package it is `/usr/bin/azor`.
- A hook runs with your rights whenever Claude Code starts. Keep `~/.claude/settings.json` and the binary writable only by you, and review hooks that tools or plugins add there.
- Names in the output come from other programs, so a process or a file could be named like an instruction. Azor marks the line as data, keeps only plain characters and cuts names short; the skill tells Claude to treat names as data.

## Other agents

Nothing in Azor is specific to Claude. Any agent that can run commands can use it:

- Give the agent the text of `azor/SKILL.md` as instructions, or as a skill if it supports the same format.
- If it can run a command when a session starts, use `azor brief` the same way as the hook above.
- For programs, `azor json now|events|snapshots|timers|health` gives versioned JSON (`"azor_json": 1`).
