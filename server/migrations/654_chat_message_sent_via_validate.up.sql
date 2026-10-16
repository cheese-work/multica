SET LOCAL lock_timeout = '2s';

ALTER TABLE chat_message VALIDATE CONSTRAINT chat_message_sent_via_check;
