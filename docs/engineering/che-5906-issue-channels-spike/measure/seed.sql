-- Synthetic scale seed for the C1 channel-list benchmark (CHE-5906). One workspace, 20 members.
-- Scale is ~5x the measured week (26.7k comments/week, 792 tagged issues/week):
-- 1.5M comments over 180 days, 10k issues, 6k 'mentioned' items for the target member.
\set ON_ERROR_STOP on
SET synchronous_commit = off;
SET session_replication_role = replica;
INSERT INTO workspace (id, name, slug) VALUES ('00000000-0000-0000-0000-00000000aaaa','bench','bench');
INSERT INTO "user" (id, name, email)
SELECT ('00000000-0000-0000-0000-0000000000' || lpad(i::text,2,'0'))::uuid, 'u'||i, 'u'||i||'@bench.test' FROM generate_series(1,20) i;
INSERT INTO member (workspace_id, user_id, role)
SELECT '00000000-0000-0000-0000-00000000aaaa', id, 'member' FROM "user" WHERE email LIKE '%@bench.test';
-- target member = user 01
INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, created_at, number)
SELECT ('10000000-0000-0000-0000-' || lpad(i::text,12,'0'))::uuid, '00000000-0000-0000-0000-00000000aaaa', 'issue '||i, 'member',
       '00000000-0000-0000-0000-000000000001', now() - make_interval(secs => random()*180*86400), i
FROM generate_series(1,10000) i;
-- roots: 525k = 35%
INSERT INTO comment (id, issue_id, author_type, author_id, content, workspace_id, created_at, updated_at)
SELECT ('20000000-0000-0000-0000-' || lpad(i::text,12,'0'))::uuid,
       ('10000000-0000-0000-0000-' || lpad((1 + (i % 10000))::text,12,'0'))::uuid,
       CASE WHEN r < 0.012 THEN 'member' ELSE 'agent' END,
       CASE WHEN r < 0.0027 THEN '00000000-0000-0000-0000-000000000001'::uuid
            WHEN r < 0.012 THEN ('00000000-0000-0000-0000-0000000000' || lpad((2 + (i % 19))::text,2,'0'))::uuid
            ELSE ('30000000-0000-0000-0000-' || lpad((i % 50)::text,12,'0'))::uuid END,
       'root '||i, '00000000-0000-0000-0000-00000000aaaa', ts, ts
FROM (SELECT i, random() r, now() - make_interval(secs => random()*180*86400) ts FROM generate_series(1,525000) i) s;
-- replies: 975k, parent = a random root of the same issue (depth 1), then 20% re-pointed at a reply (depth 2+)
INSERT INTO comment (id, issue_id, author_type, author_id, content, workspace_id, parent_id, created_at, updated_at)
SELECT ('21000000-0000-0000-0000-' || lpad(i::text,12,'0'))::uuid, c.issue_id,
       CASE WHEN r < 0.012 THEN 'member' ELSE 'agent' END,
       CASE WHEN r < 0.0027 THEN '00000000-0000-0000-0000-000000000001'::uuid
            WHEN r < 0.012 THEN ('00000000-0000-0000-0000-0000000000' || lpad((2 + (i % 19))::text,2,'0'))::uuid
            ELSE ('30000000-0000-0000-0000-' || lpad((i % 50)::text,12,'0'))::uuid END,
       'reply '||i, '00000000-0000-0000-0000-00000000aaaa', c.id,
       c.created_at + make_interval(secs => random()*3*86400), c.created_at + make_interval(secs => random()*3*86400)
FROM (SELECT i, random() r, ('20000000-0000-0000-0000-' || lpad((1 + floor(random()*525000)::int)::text,12,'0'))::uuid pid FROM generate_series(1,975000) i) s
JOIN comment c ON c.id = s.pid;
-- deepen: 20% of replies point at another reply of the same thread issue (depth 2)
UPDATE comment r SET parent_id = p.id
FROM comment p
WHERE r.id::text LIKE '21000000-%' AND (hashtext(r.id::text) % 5) = 0
  AND p.id = ('21000000-0000-0000-0000-' || lpad(((('x' || substr(md5(r.id::text),1,6))::bit(24)::int % 975000) + 1)::text,12,'0'))::uuid
  AND p.issue_id = r.issue_id AND p.created_at < r.created_at AND p.id <> r.id;
-- inbox: 400k other items, 20k non-mention items for the target, 6k 'mentioned' items for the target
INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, title, issue_id, created_at, details)
SELECT '00000000-0000-0000-0000-00000000aaaa','member',
       ('00000000-0000-0000-0000-0000000000' || lpad((2 + (i % 19))::text,2,'0'))::uuid,
       (ARRAY['new_comment','mentioned','status_changed'])[1 + (i % 3)], 't',
       ('10000000-0000-0000-0000-' || lpad((1 + (i % 10000))::text,12,'0'))::uuid, now() - make_interval(secs => random()*180*86400),
       jsonb_build_object('comment_id', ('20000000-0000-0000-0000-' || lpad((1 + (i % 525000))::text,12,'0')), 'mention_source','direct')
FROM generate_series(1,400000) i;
INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, title, issue_id, created_at, details)
SELECT '00000000-0000-0000-0000-00000000aaaa','member','00000000-0000-0000-0000-000000000001', 'new_comment','t',
       ('10000000-0000-0000-0000-' || lpad((1 + (i % 10000))::text,12,'0'))::uuid, now() - make_interval(secs => random()*180*86400),
       jsonb_build_object('comment_id', ('20000000-0000-0000-0000-' || lpad((1 + (i % 525000))::text,12,'0')))
FROM generate_series(1,20000) i;
INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, title, issue_id, created_at, details)
SELECT '00000000-0000-0000-0000-00000000aaaa','member','00000000-0000-0000-0000-000000000001', 'mentioned','t', c.issue_id, c.created_at + interval '1 minute',
       jsonb_build_object('comment_id', c.id, 'mention_source','direct')
FROM (SELECT ('20000000-0000-0000-0000-' || lpad((1 + floor(random()*525000)::int)::text,12,'0'))::uuid cid FROM generate_series(1,6000)) s
JOIN comment c ON c.id = s.cid;
ANALYZE;
SELECT 'comments', count(*) FROM comment UNION ALL SELECT 'by target', count(*) FROM comment WHERE author_id='00000000-0000-0000-0000-000000000001'
UNION ALL SELECT 'inbox', count(*) FROM inbox_item UNION ALL SELECT 'mentioned(target)', count(*) FROM inbox_item WHERE type='mentioned' AND recipient_id='00000000-0000-0000-0000-000000000001'
UNION ALL SELECT 'depth>1 replies', count(*) FROM comment r JOIN comment p ON p.id=r.parent_id WHERE p.parent_id IS NOT NULL;
