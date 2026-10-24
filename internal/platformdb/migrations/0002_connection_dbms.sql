ALTER TABLE db_connections
    ADD COLUMN dbms text NOT NULL DEFAULT 'postgresql'
    CHECK (dbms IN ('postgresql', 'mariadb'));
