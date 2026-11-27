ALTER TABLE migration_executions ADD COLUMN executed_sql text;

CREATE OR REPLACE FUNCTION migration_executions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR OLD.status IN ('success','failed')
       OR (NEW.id, NEW.seq, NEW.connection_id, NEW.migration_id, NEW.revision_id, NEW.request_id, NEW.action,
           NEW.attempt, NEW.sql, NEW.reverts_execution_id, NEW.actor_id, NEW.legacy, NEW.started_at)
          IS DISTINCT FROM
          (OLD.id, OLD.seq, OLD.connection_id, OLD.migration_id, OLD.revision_id, OLD.request_id, OLD.action,
           OLD.attempt, OLD.sql, OLD.reverts_execution_id, OLD.actor_id, OLD.legacy, OLD.started_at)
       OR (OLD.executed_sql IS NOT NULL AND NEW.executed_sql IS DISTINCT FROM OLD.executed_sql)
    THEN
        RAISE EXCEPTION 'migration history bersifat append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
