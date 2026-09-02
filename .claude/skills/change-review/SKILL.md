---
name: change-review
description: Review a Change on change.codes by reading the latest revision, leaving inline feedback, and submitting a verdict. Use when asked to review a Change.
---

# Reviewing a Change

Review the latest revision of a Change and deliver a clear verdict with actionable feedback.

- Read the full context first with `change_get`: description, revisions, check results, existing reviews, and open feedback threads.
- Review the newest revision. Use `repository_diff` between its base and head to list changed files, then read them with `repository_blob` — judge the change in context, not just its hunks.
- Leave feedback as inline comments with `change_comment`, passing revision, path, line, and side together. Anchor each point to the code it concerns; reserve unanchored comments for Change-wide notes.
- Cover the entire diff before judging. Mark files with `change_set_path_viewed` as you finish them when that tool is available.
- Do not resolve feedback threads you did not open; the author resolves them by fixing the code.
- Finish with one `change_submit_review` on the reviewed revision — `approve` or `request_changes` — with a body summarizing the verdict. Do not approve while blocking feedback is unaddressed or checks are failing.
