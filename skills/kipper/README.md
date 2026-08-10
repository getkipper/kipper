# Kipper skill

`SKILL.md` in this folder turns a Claude Code or Codex session into a Kipper expert. It teaches the agent the full `kip` command surface, the mental model (projects, environments, apps, services, functions, jobs), when to use `kip` versus `kubectl`, how to install a cluster, and where the docs live.

Everything in it is public and generic. No cluster credentials, IPs, or private hosts, so it's safe to share.

## Use it with Claude Code

Claude Code auto-loads skills from a `.claude/skills/` directory. Copy or symlink this folder into yours:

```bash
# Per project (skill available in this repo's sessions)
mkdir -p .claude/skills
cp -r skills/kipper .claude/skills/kipper

# Or for every project on your machine
mkdir -p ~/.claude/skills
ln -s "$(pwd)/skills/kipper" ~/.claude/skills/kipper
```

Start a session and the skill activates whenever your task mentions Kipper, the `kip` command, a `kipper.yaml`, or a `*.kipper.run` cluster. You can also invoke it explicitly with `/kipper`.

## Use it with Codex

Codex reads project instructions from `AGENTS.md`. Point it at this skill by adding a line to your `AGENTS.md`:

```markdown
For any Kipper or `kip` task, read and follow skills/kipper/SKILL.md.
```

Codex will open the file when a Kipper task comes up. Alternatively, paste the contents of `SKILL.md` directly into your `AGENTS.md`.

## Use it with any assistant

`SKILL.md` is plain Markdown. Any assistant that can read a file or accept pasted context can use it. The body is self-contained, so dropping it into a system prompt or a context file works too.

## Keeping it current

The command reference is based on the `kip` CLI's help output. If you add or change commands, refresh it by walking `kip <command> --help` and updating `SKILL.md` to match. Docs links point at `https://getkipper.com/en/<page>`.
