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
CREATE INDEX IF NOT EXISTS profiles_closures_retention_idx ON profiles.closures(updated_at,id) WHERE state IN('closed','cancelled');

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
  SELECT id FROM profiles.closures WHERE state IN('closed','cancelled') AND updated_at<effective_time-interval '12 months'
  ORDER BY updated_at,id LIMIT batch_limit FOR UPDATE SKIP LOCKED
 ) INTO expired_ids;

 DELETE FROM profiles.closure_events WHERE closure_id=ANY(expired_ids);
 GET DIAGNOSTICS closure_events_deleted = ROW_COUNT;
 DELETE FROM profiles.closures WHERE id=ANY(expired_ids);
 GET DIAGNOSTICS closures_deleted = ROW_COUNT;
 RETURN NEXT;
END;
$$;
REVOKE EXECUTE ON FUNCTION profiles.cleanup_expired(timestamptz,integer) FROM PUBLIC;
