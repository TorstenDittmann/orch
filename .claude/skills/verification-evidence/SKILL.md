---
name: verification-evidence
description: Record verification evidence and agent provenance on a Change revision after testing, benchmarking, or otherwise verifying work.
---

# Verification evidence

Attach proof of verification to the exact revision it validates.

- Submit evidence with `evidence_submit` after verifying, choosing the kind that matches the claim: `test` for test runs, `screenshot` or `recording` for UI behavior, `benchmark` for performance, `security` for security checks, `rollout` for deployment verification, `other` otherwise.
- Report honest results — `pass`, `fail`, or `info`. Submit failures too; evidence is a record, not advocacy.
- Name each item for what was verified and summarize how it was verified; link supporting artifacts with `url` when one exists.
- Evidence attaches to a specific revision. After pushing a new revision, re-verify and submit fresh evidence; earlier evidence does not carry forward.
- When you did the work as an agent, attach your session to the revision with `provenance_attach`.
- If the evidence tools are not available in your session, record the verification results in a Change comment instead.
