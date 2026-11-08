package db

// Conn returns the executor the queries run on, so a caller can run its own
// SQL on the same transaction as a WithTx query set.
func (q *Queries) Conn() DBTX { return q.db }
