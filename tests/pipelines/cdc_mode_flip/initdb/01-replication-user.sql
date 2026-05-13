-- Grant binlog replication and full read on all schemas to the sling_repl user.
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'sling_repl'@'%';
GRANT SELECT ON *.* TO 'sling_repl'@'%';
FLUSH PRIVILEGES;
