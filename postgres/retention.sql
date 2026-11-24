ALTER TABLE profiles.requests ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS profiles_requests_created_idx ON profiles.requests(created_at);
ALTER TABLE profiles.closure_requests ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS profiles_closure_requests_created_idx ON profiles.closure_requests(created_at);
ALTER TABLE profiles.closures ADD COLUMN IF NOT EXISTS updated_at timestamptz;
-- Preserve at least the promised retention when older history lacks a cancellation event.
UPDATE profiles.closures c SET updated_at=CASE WHEN c.state='cancelled' THEN
 COALESCE((SELECT max(e.created_at) FROM profiles.closure_events e WHERE e.closure_id=c.id AND e.action='cancel'),current_timestamp)
 ELSE COALESCE((c.result->>'completed_at')::timestamptz,c.created_at) END WHERE c.updated_at IS NULL;
ALTER TABLE profiles.closures ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE profiles.closures ALTER COLUMN updated_at SET DEFAULT now();

-- Seed existing terminal rows once. New terminal transitions use the database
-- clock, independent of the timestamps the application stores in updated_at.
ALTER TABLE profiles.closures ADD COLUMN IF NOT EXISTS retention_terminal_at timestamptz;
UPDATE profiles.closures SET retention_terminal_at=CASE WHEN isfinite(updated_at)
 THEN LEAST(updated_at,statement_timestamp()) ELSE statement_timestamp() END
 WHERE state IN('closed','cancelled') AND retention_terminal_at IS NULL;
DROP INDEX IF EXISTS profiles.profiles_closures_retention_idx;
CREATE INDEX IF NOT EXISTS profiles_closures_terminal_retention_idx
 ON profiles.closures(retention_terminal_at,id) WHERE state IN('closed','cancelled');

CREATE OR REPLACE FUNCTION profiles.guard_closure_row() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF (NEW.id,NEW.issuer,NEW.subject,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.issuer,OLD.subject,OLD.created_at) THEN
   RAISE EXCEPTION 'immutable_closure_identity' USING ERRCODE='42501';
  END IF;
  IF NEW.retention_terminal_at IS DISTINCT FROM OLD.retention_terminal_at OR
     (OLD.retention_terminal_at IS NOT NULL AND NEW.state IS DISTINCT FROM OLD.state) THEN
   RAISE EXCEPTION 'immutable_closure_retention' USING ERRCODE='42501';
  END IF;
 ELSIF NEW.retention_terminal_at IS NOT NULL THEN
  RAISE EXCEPTION 'database_owned_closure_retention' USING ERRCODE='42501';
 END IF;
 IF NEW.state IN('closed','cancelled') AND NEW.retention_terminal_at IS NULL THEN
  NEW.retention_terminal_at := statement_timestamp();
 END IF;
 RETURN NEW;
END;
$$;
REVOKE EXECUTE ON FUNCTION profiles.guard_closure_row() FROM PUBLIC;
CREATE OR REPLACE TRIGGER profiles_closures_guard_row BEFORE INSERT OR UPDATE ON profiles.closures
 FOR EACH ROW EXECUTE FUNCTION profiles.guard_closure_row();

-- The application may use an earlier processing time, but setting it in
-- the future must never advance expiry beyond the database clock.
CREATE OR REPLACE FUNCTION profiles.expire_profile_request(owner_issuer text, owner_subject text, request_key text, requested_at timestamptz)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DELETE FROM profiles.requests
 WHERE issuer=owner_issuer AND subject=owner_subject AND key=request_key
 AND created_at<LEAST(requested_at,statement_timestamp())-interval '24 hours';
$$;
REVOKE EXECUTE ON FUNCTION profiles.expire_profile_request(text,text,text,timestamptz) FROM PUBLIC;

CREATE OR REPLACE FUNCTION profiles.expire_closure_request(owner_issuer text, owner_subject text, request_key text, requested_at timestamptz)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DELETE FROM profiles.closure_requests
 WHERE actor_issuer=owner_issuer AND actor_subject=owner_subject AND key=request_key
 AND created_at<LEAST(requested_at,statement_timestamp())-interval '24 hours';
$$;
REVOKE EXECUTE ON FUNCTION profiles.expire_closure_request(text,text,text,timestamptz) FROM PUBLIC;

-- Called in the closure transaction after both account and application are closed.
CREATE OR REPLACE FUNCTION profiles.erase_closed_account_requests(owner_issuer text, owner_subject text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS (
  SELECT 1 FROM profiles.profiles p JOIN profiles.closures c USING (issuer,subject)
  WHERE p.issuer=owner_issuer AND p.subject=owner_subject AND p.account_state='closed' AND c.state='closed'
 ) THEN
  RAISE EXCEPTION 'closure_required' USING ERRCODE='22023';
 END IF;
 DELETE FROM profiles.requests WHERE issuer=owner_issuer AND subject=owner_subject;
 DELETE FROM profiles.closure_requests WHERE actor_issuer=owner_issuer AND actor_subject=owner_subject;
END;
$$;
REVOKE EXECUTE ON FUNCTION profiles.erase_closed_account_requests(text,text) FROM PUBLIC;

ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS closed_account_at timestamptz;
WITH recovered AS (
UPDATE profiles.profiles p SET closed_account_at=(c.result->>'completed_at')::timestamptz FROM profiles.closures c
 WHERE p.account_state='closed' AND p.closed_account_at IS NULL AND p.issuer=c.issuer AND p.subject=c.subject
 AND c.state='closed' AND c.result->>'completed_at' IS NOT NULL
 RETURNING c.id,p.issuer,p.subject,p.closed_account_at
)
INSERT INTO profiles.closure_completions(closure_id,issuer,subject,completed_at)
 SELECT id,issuer,subject,closed_account_at FROM recovered ON CONFLICT DO NOTHING;
CREATE INDEX IF NOT EXISTS profiles_closed_expiry ON profiles.profiles(closed_account_at,issuer,subject) WHERE closed_account_at IS NOT NULL;

CREATE OR REPLACE FUNCTION profiles.cleanup_expired(requested_at timestamptz, batch_limit integer)
RETURNS TABLE(requests_deleted integer, closure_requests_deleted integer, closures_deleted integer, closure_events_deleted integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog SET TimeZone='UTC' AS $$
DECLARE
 effective_time timestamptz;
 expired_ids text[];
BEGIN
 IF requested_at IS NULL OR NOT isfinite(requested_at) OR batch_limit IS NULL OR batch_limit NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'invalid_retention_input' USING ERRCODE='22023';
 END IF;
 effective_time := LEAST(requested_at,statement_timestamp());

 DELETE FROM profiles.requests WHERE ctid IN (
  SELECT ctid FROM profiles.requests WHERE created_at<effective_time-interval '24 hours'
  ORDER BY created_at,issuer,subject,key LIMIT batch_limit FOR UPDATE SKIP LOCKED
 );
 GET DIAGNOSTICS requests_deleted = ROW_COUNT;

 DELETE FROM profiles.closure_requests WHERE ctid IN (
  SELECT ctid FROM profiles.closure_requests WHERE created_at<effective_time-interval '24 hours'
  ORDER BY created_at,actor_issuer,actor_subject,key LIMIT batch_limit FOR UPDATE SKIP LOCKED
 );
 GET DIAGNOSTICS closure_requests_deleted = ROW_COUNT;

 SELECT ARRAY(
  SELECT id FROM profiles.closures WHERE state IN('closed','cancelled') AND retention_terminal_at<effective_time-interval '12 months'
  ORDER BY retention_terminal_at,id LIMIT batch_limit FOR UPDATE SKIP LOCKED
 ) INTO expired_ids;

 DELETE FROM profiles.closure_events WHERE id IN (
  SELECT id FROM profiles.closure_events WHERE closure_id=ANY(expired_ids)
  ORDER BY closure_id,id LIMIT batch_limit
 );
 GET DIAGNOSTICS closure_events_deleted = ROW_COUNT;
 DELETE FROM profiles.closures c WHERE c.id=ANY(expired_ids)
  AND NOT EXISTS (SELECT 1 FROM profiles.closure_events e WHERE e.closure_id=c.id);
 GET DIAGNOSTICS closures_deleted = ROW_COUNT;
 -- Preserve the owner while completion delivery or avatar release is outstanding.
 WITH due AS (
   SELECT issuer,subject FROM profiles.profiles p WHERE account_state='closed'
   AND ((closed_account_at AT TIME ZONE 'Asia/Shanghai')+interval '12 months') AT TIME ZONE 'Asia/Shanghai' <= effective_time
   AND NOT EXISTS(SELECT 1 FROM profiles.closure_completions c WHERE c.issuer=p.issuer AND c.subject=p.subject)
   AND NOT EXISTS(SELECT 1 FROM profiles.avatar_releases v WHERE v.issuer=p.issuer AND v.subject=p.subject)
   ORDER BY closed_account_at,issuer,subject LIMIT batch_limit FOR UPDATE SKIP LOCKED
 ), old_events AS (
   SELECT e.id FROM profiles.events e JOIN due USING(issuer,subject) ORDER BY e.id LIMIT batch_limit
 ), removed AS (DELETE FROM profiles.events e USING old_events WHERE e.id=old_events.id)
 DELETE FROM profiles.profiles p USING due WHERE p.issuer=due.issuer AND p.subject=due.subject
   AND NOT EXISTS(SELECT 1 FROM profiles.events e WHERE e.issuer=p.issuer AND e.subject=p.subject);
 RETURN NEXT;
END;
$$;
REVOKE EXECUTE ON FUNCTION profiles.cleanup_expired(timestamptz,integer) FROM PUBLIC;
