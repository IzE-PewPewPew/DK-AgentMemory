# Development docs

Internal. Not part of the published documentation.

- [STATUS.md](STATUS.md) — what is built, what is verified, and what is not
- [BUILD-PLAN.md](BUILD-PLAN.md) — milestones, 38 tasks, acceptance criteria
- [AGENT-PROMPTS.md](AGENT-PROMPTS.md) — paste-ready prompts for building with a coding agent

Start at STATUS.md. It is the only one of the three that describes the code as
it actually is.

## Order of work

```
M1  Foundation & server core      2 weeks    two tools share a memory
M2  Client & agent integration    1.5 weeks  one command wires everything
M3  Import & project identity     1 week     real history, correctly grouped
M4  Team & sharing                1 week     private stays private
M5  Consolidation, graph, offline 2 weeks    lessons nobody typed
M6  Release & launch              1 week     a stranger succeeds in 10 minutes
```

Do not start a milestone until the previous one's acceptance test passes.

## Before going public

- [ ] Running your own team for at least a month
- [ ] `gitleaks detect --log-opts="--all"` clean
- [ ] No internal hostnames anywhere in history
- [ ] Install path tested on a machine that has never seen the project
- [ ] SECURITY.md contact is monitored
- [ ] Actions pinned to SHAs
