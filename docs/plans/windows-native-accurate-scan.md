# Windows-Native Accurate Scan Implementation Plan

> For Hermes: implement task-by-task with TDD and independent review.

Goal: Produce point-in-time Windows folder measurements matching Explorer logical Size as closely as filesystem semantics allow, while also recording allocated Size on disk with bounded resource use.

Architecture: On Windows, create a short-lived VSS ClientAccessible snapshot per volume, scan its GLOBALROOT device path, map results back to the configured drive paths, then delete the shadow copy in a guaranteed cleanup path. Enumerate in a streaming single walker, never follow reparse points, collect logical and allocated bytes per visible ancestor, allow only one scan at a time, and persist one folder row per scan. Non-Windows builds use a no-op snapshot provider and logical size as allocated size for testability.

Tech stack: Go, Win32 kernel32 GetCompressedFileSizeW, PowerShell/CIM Win32_ShadowCopy, SQLite.

Tasks:
1. Add validated accuracy/resource config: VSS mode, allocated-size collection, scan timeout, and single scan guard.
2. Add platform-specific VSS provider with command timeouts, strict drive validation, JSON parsing, and guaranteed deletion.
3. Refactor scanner to distinguish source root from reported root and accumulate logical plus allocated bytes into every ancestor.
4. Extend SQLite schema additively for allocated bytes and snapshot consistency metadata.
5. Integrate VSS lifecycle and non-overlapping scans in the scheduler.
6. Expose logical/allocated values and consistency status in API/dashboard documentation.
7. Verify unit/integration tests, race detector, vet, Linux build, Windows cross-build, and independent review.

Acceptance criteria:
- VSS source is used on Windows when enabled; failure is explicit and does not silently claim an exact scan.
- Every created shadow copy is deleted after scanning, including scan/persistence errors.
- One scan at a time; overlapping triggers are skipped with an actionable log.
- Logical size sums file EOF lengths; allocated size uses GetCompressedFileSizeW on Windows.
- Junctions/symlinks/reparse points are not traversed.
- Folder paths stored in SQLite remain original configured paths, never GLOBALROOT paths.
- Existing databases migrate without data loss.
- Dashboard distinguishes Size and Size on disk and reports snapshot consistency.
