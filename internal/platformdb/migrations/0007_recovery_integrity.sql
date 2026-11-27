ALTER TABLE db_connections ADD COLUMN identity_epoch uuid NOT NULL DEFAULT gen_random_uuid();

CREATE FUNCTION rotate_connection_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.host, NEW.port, NEW.dbname, NEW.dbms, NEW.deleted_at)
       IS DISTINCT FROM (OLD.host, OLD.port, OLD.dbname, OLD.dbms, OLD.deleted_at) THEN
        NEW.identity_epoch := gen_random_uuid();
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER rotate_connection_identity BEFORE UPDATE ON db_connections
    FOR EACH ROW EXECUTE FUNCTION rotate_connection_identity();

ALTER TABLE migration_requests ADD COLUMN target_identity text NOT NULL DEFAULT '',
    ADD COLUMN target_server text NOT NULL DEFAULT '';
ALTER TABLE migration_executions ADD COLUMN target_identity text NOT NULL DEFAULT '',
    ADD COLUMN target_server text NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN dispatch_ready boolean NOT NULL DEFAULT false;
UPDATE jobs SET dispatch_ready = true WHERE attempt_open;
CREATE INDEX jobs_target_server_idx ON jobs ((plan->>'TargetServer'),status);

UPDATE jobs SET error_message = 'Detail kegagalan lama tidak tersedia.' WHERE error_message IS NOT NULL;
UPDATE migrations SET error_message = 'Detail kegagalan lama tidak tersedia.' WHERE error_message IS NOT NULL;
ALTER TABLE migration_executions DISABLE TRIGGER migration_executions_guard;
UPDATE migration_executions SET error_message = 'Detail kegagalan lama tidak tersedia.' WHERE error_message IS NOT NULL;
ALTER TABLE migration_executions ENABLE TRIGGER migration_executions_guard;
