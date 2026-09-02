---
name: address-feedback
description: Address review feedback on a Change by replying to threads, fixing in a new revision, and resolving only what is fixed. Use after a review requests changes.
---

# Addressing review feedback

Work through review feedback so every thread ends in a fix or an agreed answer.

- Read `change_get` first to collect open feedback threads, review verdicts, and check state for the latest revision.
- Handle every thread. Reply with `change_reply_feedback` saying what you changed or why you disagree — never leave a thread unanswered.
- Make fixes on the attached branch and push them, producing a new revision; verify checks pass on it.
- Resolve a thread with `change_resolve_feedback` only after the fix is in a pushed revision. A reply or a promise is not resolution. Use `change_reopen_feedback` if a resolved issue resurfaces.
- If you disagree with feedback, say why in a reply and leave the thread open for the reviewer to settle.
- When every thread is handled and checks pass, re-request the reviewer with `change_request_reviewer`.
