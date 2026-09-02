---
name: repo-planning
description: Create and maintain implementation plans as Plans on change.codes. Use when planning repository work that should be reviewable, versioned, or handed off before implementation.
---

# Repository planning

Turn an implementation request into a plan that another contributor can execute, stored as a Plan on the repository at change.codes — not as a Markdown file in the working tree.

- Inspect the repository's instructions, architecture, relevant code, tests, and current working tree before writing the plan.
- Use the change.codes MCP tools to manage Plans: `plans_list` to see existing Plans; `plan_get` to read history, discussion, approval requests, and linked Changes; `plan_push` to create or revise; `plan_comment` to discuss; and `plan_request_approval` when it is ready for a human decision. Without MCP, use the matching `change plan …` CLI commands.
- Do not approve a Plan on the user's behalf. After a human approves it, start implementation with `change_create` and its `planNum` argument (or `change change create --plan NUMBER`) so the Change is pinned to the approved Plan version.
- List existing plans first. When one covers the same work, push a new version of it instead of creating a duplicate; versions preserve history. Never repurpose an unrelated plan.
- Make the plan self-contained: state the outcome, relevant context, scope, constraints, and acceptance criteria.
- Break implementation into ordered, verifiable tasks. Name concrete files or components when known and identify tests or checks for each meaningful stage.
- Record important decisions, risks, dependencies, migrations, rollout concerns, and unresolved questions. Clearly distinguish confirmed facts from assumptions.
- Keep the plan aligned with the user's requested scope. Do not implement the plan unless the user also asks for implementation.
- Before finishing, verify that every referenced path exists or is explicitly marked as a file to create, then report the plan's number and title.
