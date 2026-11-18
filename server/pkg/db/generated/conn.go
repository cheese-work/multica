package db

// Conn is the connection or transaction these queries run on, for readers that
// take a DBTX (intakehold.Load) and must see the caller's transaction.
func (q *Queries) Conn() DBTX { return q.db }
