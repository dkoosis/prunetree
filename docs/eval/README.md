# Snipe Evaluation Framework

Scenario-based evaluation to measure whether snipe helps Claude complete Go development tasks effectively.

## Evaluation Dimensions

| Dimension | What We Measure |
|-----------|-----------------|
| **Task Completion** | Did Claude finish the task? |
| **Accuracy** | Was the answer correct? |
| **Context Quality** | Did snipe provide the right context? |
| **Efficiency** | How many commands to reach the answer? |

## Running a Scenario

1. Start a fresh Claude session
2. Load the scenario setup (paste error output, specify entry point, etc.)
3. Ask Claude to complete the goal using snipe
4. Record results in `results/`

## Recording Results

Create a file in `results/` named `{scenario}-{date}.md`:

```markdown
# Scenario: find-bug
Date: 2025-01-15
Model: claude-sonnet-4-20250514

## Outcome
- Task Completed: yes/no
- Accurate: yes/no
- Commands Used: def, callers, refs
- Total Queries: 5

## Friction Log
- [pain point observed]
- [unexpected behavior]

## Notes
[observations, suggestions]
```

## Scenarios

| Scenario | Purpose |
|----------|---------|
| [find-bug](scenarios/find-bug.md) | Debug from error output |
| [understand-flow](scenarios/understand-flow.md) | Trace data through functions |
