-- CHE-7409: retire the global removal-node index of 999r; 99h replaces it.
DROP INDEX CONCURRENTLY IF EXISTS merge_queue_intake_removal_uidx;
