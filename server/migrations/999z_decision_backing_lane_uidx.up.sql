CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_backing_lane_member_topic_uidx ON decision_backing_lane (workspace_id, creator_id, topic_id);
