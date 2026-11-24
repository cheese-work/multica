// Package oomwatch is the OOM classifier and the kernel-journal reader of the
// X99 containment plan (CHE-7168 r13, section 3.5, step 9a).
//
// It has three parts:
//
//   - Classifier: reads kernel message lines in journal order and turns each
//     `Killed process` record into a class (contained, foreign_memcg) or a hold
//     cause. A record is classified only from its own constraint line.
//   - Reader: processes journal entries from the exact production filter,
//     drops history and other boots, feeds the classifier, and checks the 30 s
//     heartbeat nonces (each must be seen within 10 s).
//   - PlanStart and the checkpoint file: the cursor and boot-gap rules at
//     reader start. TailCursor, CursorValid and Follow run the read-only
//     journalctl queries and the production follower.
//
// Later steps build on this package: 9b (count baseline), 9c (deficit clock,
// live gap marker) and 10a (the durable hold). Here a hold is a call to
// Sinks.Hold; this package never latches anything itself.
package oomwatch
