SET LOCAL lock_timeout = '2s';

DROP TABLE IF EXISTS merge_queue_evidence;
DROP TABLE IF EXISTS merge_queue_state;
DROP TABLE IF EXISTS merge_queue_enqueue_event;
DROP TABLE IF EXISTS merge_queue_pr_close;
DROP TABLE IF EXISTS merge_queue_wait;
DROP TABLE IF EXISTS merge_queue_association;
DROP TABLE IF EXISTS merge_queue_epoch;
DROP TABLE IF EXISTS merge_queue_intake;
