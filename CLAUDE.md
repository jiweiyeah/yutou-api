# CLAUDE.md — Project Conventions for new-api

## MANDATORY: Read AGENTS.md with the Read tool

Do not treat `@AGENTS.md` as loaded. Claude Code does not reliably inline that import.

Before any planning, coding, reviewing, or answering a project question, you MUST call the Read tool on the repo-root file `AGENTS.md` and wait for the full contents. This is the first action of every session and every new task.

Rules:

- Do not start from memory, summaries, or this file alone.
- Do not skip the Read because a previous turn mentioned AGENTS.md.
- Do not replace the Read with a grep, glob, or partial skim.
- After reading, follow every rule in `AGENTS.md` for the rest of the work.
- If the task touches `web/`, also Read `web/AGENTS.md` before editing frontend files.
- If the task touches billing as defined under **Billing rules (mandatory read gate)** in `AGENTS.md`, also Read `.agents/rules/billing.md` in full before planning or editing. Tasks outside that definition may skip it.

<!-- ===== CUSTOM START: fork-only rules, not present in upstream AGENTS.md ===== -->

## Fork & Upstream Sync Workflow (CRITICAL — read before any branch op)

This repo is a **fork** of `QuantumNous/new-api`. Two remotes exist:

- `origin`   → `https://github.com/jiweiyeah/yutou-api.git` (our fork; deploys come from here)
- `upstream` → `https://github.com/QuantumNous/new-api.git` (the official source; read-only for us)

### Branch roles (do not violate)

- **`main`** — pure mirror of `upstream/main`. **NEVER write a single line of custom code on `main`.** It only receives `git merge upstream/main` and is pushed to `origin/main`.
- **`custom`** — long-lived branch holding all our customizations. **All custom development, deploys, and releases come from `custom`.** Feature branches branch off `custom` and merge back via PR/merge.

### Syncing official updates (the standard procedure)

```bash
# 1. Refresh main from upstream (no edits, fast-forward only ideally)
git checkout main
git fetch upstream
git merge --ff-only upstream/main   # if not FF, investigate before forcing
git push origin main

# 2. Bring official changes into custom
git checkout custom
git merge main                       # ALWAYS merge, NEVER rebase custom
# resolve conflicts → commit
git push origin custom
```

**Why merge, not rebase, on `custom`:** `custom` is a shared, long-lived, deployed branch. Rebasing rewrites history and forces `--force` push, which corrupts collaborators' clones and deploy pipelines. Short-lived feature branches off `custom` MAY rebase before merging back; `custom` itself MUST NOT.

### Customization style — minimize merge conflicts

Conflicts come from editing files the upstream also edits. Rules of engagement:

1. **Prefer adding new files over modifying official files.** New routes → `router/custom_*.go`; new middleware → `middleware/custom_*.go`; new handlers → `controller/custom_*.go`. Wire them in at a single, well-known registration point.
2. **Externalize behavior via env vars / config** rather than patching defaults inline.
3. **When an official file MUST be modified**, wrap the change with explicit markers so it is greppable and survives 3-way merges:
   ```go
   // ===== CUSTOM START: <short reason> =====
   ...custom code...
   // ===== CUSTOM END =====
   ```
   Same pattern in TS/JS (`// ===== CUSTOM START =====`), JSX/HTML (`{/* CUSTOM START */}`), YAML/Dockerfile (`# ===== CUSTOM START =====`).
4. **Never edit upstream-maintained files casually** (e.g., `go.mod` upstream deps, `package.json` core deps, CI workflows under `.github/workflows/` that upstream owns) without a CUSTOM marker block.

### Sync cadence

- Run the sync procedure **at least weekly**. Letting upstream drift for months produces exponential conflict cost.
- Sync **immediately** when upstream ships a security fix or a breaking change you depend on.

### Forbidden actions

- ❌ Committing custom code on `main`.
- ❌ `git push --force` on `main` or `custom`.
- ❌ `git rebase` on `main` or `custom`.
- ❌ Deleting/renaming `upstream` remote.
- ❌ Modifying official files without `// CUSTOM START/END` markers.

### Quick verification

```bash
git remote -v          # must show origin (fork) AND upstream (QuantumNous)
git branch --show-current   # custom for dev work; main only for sync
```

## Production SSH (custom)

How to log in, and how to enroll a new laptop key: `docs/custom/ssh.md`.
Host `154.202.119.148`, port **58317**, key-only. Never commit private keys.

<!-- ===== CUSTOM END ===== -->
